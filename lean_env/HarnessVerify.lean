import Lean

/-!
# harness_verify: trusted final check of a job's theorems

Usage: `harness_verify TRUSTED_ROOTS SpecModule JobModule name₁ name₂ …`

* `TRUSTED_ROOTS` is a `:`-separated list of directories holding read-only,
  trusted `.olean` files (toolchain, Mathlib, Harness).
* `SpecModule` was compiled from the confirmed statements only
  (`theorem nameᵢ <signature> := sorry`, importing nothing but Mathlib).
* `JobModule` was compiled from the job's final Lean file.

For every `nameᵢ` this program checks, using only data loaded from `.olean`
files and never running code from them (initializers stay disabled and
extension state is not loaded):

1. `nameᵢ` is a `theorem` declared in the job module (not an axiom, opaque
   or def, and not imported from elsewhere);
2. its type and universe parameters are syntactically identical to the
   spec's, so the job proved exactly the confirmed statement (a job cannot
   redefine notation or instances to change what the statement means);
3. every axiom it transitively depends on is one of `propext`,
   `Classical.choice`, `Quot.sound`.

Axioms are collected by walking the constants themselves (types, values,
constructors) over the whole dependency closure. Lean's cached per-module
axiom data is NOT used: the job's `.olean` could forge it. Every module the
job imports (transitively) must live under a trusted root, so a job cannot
smuggle in a module of its own; `leanchecker` re-checks only the job
module, so this restriction is what keeps every proof kernel-checked.

Kernel re-checking of the proofs is done separately by the toolchain's
`leanchecker`. Output: one JSON object per name on stdout; exit code 0 iff
all names pass.
-/

open Lean

def allowedAxioms : List Name := [``propext, ``Classical.choice, ``Quot.sound]

/-- Index of the job module. Fails if any other module lives outside the
trusted roots. -/
def checkImports (env : Environment) (jobMod : Name) (roots : Array System.FilePath) :
    IO Nat := do
  let some jobIdx := env.getModuleIdx? jobMod
    | throw <| IO.userError s!"job module {jobMod} not loaded"
  for m in env.header.moduleNames, i in [0:env.header.moduleNames.size] do
    if i == jobIdx.toNat then continue
    let path ← IO.FS.realPath (← findOLean m)
    unless roots.any (fun r => (r.toString ++ "/").isPrefixOf path.toString) do
      throw <| IO.userError s!"untrusted module {m} imported from {path}"
  return jobIdx.toNat

/-- Axioms reachable from `root`, by walking every constant in its
dependency closure. Iterative, so deep chains cannot overflow the stack.
Returns the axioms and the number of constants visited. -/
def trustedAxioms (env : Environment) (root : Name) : Array Name × Nat := Id.run do
  let mut visited : NameHashSet := {}
  let mut found : NameHashSet := {}
  let mut stack : Array Name := #[root]
  while true do
    let some c := stack.back? | break
    stack := stack.pop
    if visited.contains c then continue
    visited := visited.insert c
    match env.find? c with
    | none => found := found.insert c -- dangling reference: report, never trust
    | some ci =>
      if ci matches .axiomInfo _ then
        found := found.insert c
      for d in ci.type.getUsedConstants do
        stack := stack.push d
      if let some v := ci.value? (allowOpaque := true) then
        for d in v.getUsedConstants do
          stack := stack.push d
      if let .inductInfo iv := ci then
        for d in iv.ctors do
          stack := stack.push d
  return (found.toArray.qsort Name.lt, visited.size)

def checkOne (spec job : Environment) (jobIdx : Nat) (n : Name) : Json := Id.run do
  let fail (msg : String) : Json :=
    Json.mkObj [("name", toJson n.toString), ("ok", toJson false), ("error", toJson msg)]
  let some sci := spec.find? n | return fail "not in spec"
  let some jci := job.find? n | return fail "not found in job"
  let .thmInfo jt := jci | return fail "not a theorem in job"
  if (job.getModuleIdxFor? n).map (·.toNat) != some jobIdx then
    return fail "not declared in the job module"
  if jt.levelParams != sci.levelParams then
    return fail "universe parameters differ from the confirmed statement"
  if jt.type != sci.type then
    return fail "type differs from the confirmed statement"
  let (axs, walked) := trustedAxioms job n
  let bad := axs.filter (fun a => !allowedAxioms.contains a)
  let base := [("name", toJson n.toString), ("axioms", toJson (axs.map (·.toString))),
    ("walked", toJson walked)]
  if !bad.isEmpty then
    return Json.mkObj (base ++ [("ok", toJson false),
      ("error", toJson s!"forbidden axioms: {bad.toList}")])
  return Json.mkObj (base ++ [("ok", toJson true)])

def main (args : List String) : IO UInt32 := do
  let rootsArg :: specMod :: jobMod :: names := args
    | IO.eprintln "usage: harness_verify TRUSTED_ROOTS SpecModule JobModule name..."; return 2
  if names.isEmpty then
    IO.eprintln "no theorem names given"; return 2
  initSearchPath (← findSysroot)
  let roots ← ((rootsArg.splitOn ":").filter (· != "")).toArray.mapM (IO.FS.realPath ⟨·⟩)
  let t0 ← IO.monoMsNow
  let phase (what : String) : IO Unit := do
    if (← IO.getEnv "HARNESS_VERIFY_TIMING").isSome then
      IO.eprintln s!"[{(← IO.monoMsNow) - t0} ms] {what}"
  -- leakEnv: never free the environments (freeing Mathlib twice takes
  -- about a minute and the process exits right after anyway).
  let spec ← importModules #[{ module := specMod.toName }] {} (loadExts := false) (leakEnv := true)
  phase "imported spec"
  let job ← importModules #[{ module := jobMod.toName }] {} (loadExts := false) (leakEnv := true)
  phase "imported job"
  let jobIdx ← try checkImports job jobMod.toName roots catch e =>
    IO.println (Json.mkObj [("ok", toJson false), ("error", toJson e.toString)]).compress
    return 1
  phase "checked imports"
  let mut allOk := true
  for n in names do
    let r := checkOne spec job jobIdx n.toName
    IO.println r.compress
    phase s!"checked {n}"
    if r.getObjValD "ok" != toJson true then
      allOk := false
  phase "checked"
  return if allOk then 0 else 1
