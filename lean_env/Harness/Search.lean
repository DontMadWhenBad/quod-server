import Lean

/-!
# Lemma search for the prover's tools

`#harness_search "frag₁ frag₂ …" N` lists up to `N` (default 20, max 50)
theorems whose full name contains every fragment (case-insensitive),
shortest names first, each with its statement. The LLM uses it (through the
`search_lemmas` tool) to find real Mathlib names instead of guessing them.
-/

namespace Harness

open Lean Elab Command

syntax (name := harnessSearch) "#harness_search " str (ppSpace num)? : command

/-- `s` contains `frag` (both already lower-case, `frag` non-empty). -/
private def containsFrag (s frag : String) : Bool :=
  (s.splitOn frag).length > 1

@[command_elab harnessSearch]
def elabHarnessSearch : CommandElab
  | `(#harness_search $q:str $[$n:num]?) => do
    let limit := min 50 ((n.map (·.getNat)).getD 20)
    let frags := (q.getString.toLower.splitOn " ").filter (· ≠ "")
    if frags.isEmpty then
      throwError "empty query"
    let env ← getEnv
    let hits : Array Name := env.constants.fold (init := #[]) fun acc name ci =>
      if ci matches .thmInfo _ && !name.isInternalDetail &&
          frags.all (containsFrag name.toString.toLower) then
        acc.push name
      else acc
    if hits.isEmpty then
      logInfo m!"no theorem name contains all of {frags}"
      return
    let sorted := hits.qsort fun a b =>
      let la := a.toString.length
      let lb := b.toString.length
      la < lb || (la == lb && Name.lt a b)
    let shown := sorted.extract 0 limit
    let mut lines : Array MessageData := #[]
    for h in shown do
      let ci ← getConstInfo h
      lines := lines.push m!"{h} : {ci.type}"
    let more := if hits.size > limit then m!"\n… {hits.size - limit} more; add fragments to narrow" else m!""
    logInfo (MessageData.joinSep lines.toList "\n" ++ more)
  | _ => throwUnsupportedSyntax

end Harness
