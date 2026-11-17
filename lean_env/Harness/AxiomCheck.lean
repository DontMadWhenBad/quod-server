import Lean

/-!
# Axiom check for LLM-generated proofs

`#harness_axioms foo` collects every axiom that `foo` transitively depends on
and compares it against an allowlist.

* All allowed → info message  `HARNESS_AXIOMS_OK <name>: [...]`
* Otherwise   → error message `HARNESS_AXIOMS_BAD <name>: [...]` (offending axioms only)

This catches `sorry` (`sorryAx`), `native_decide` (`Lean.ofReduceBool`),
and any `axiom` the model declared itself. The server must treat the
proof as verified only if the file compiles with no errors AND the
`HARNESS_AXIOMS_OK` line is present.
-/

namespace Harness

open Lean Elab Command

/-- The standard axioms of Lean + Mathlib classical mathematics. -/
def allowedAxioms : List Name :=
  [``propext, ``Classical.choice, ``Quot.sound]

syntax (name := harnessAxioms) "#harness_axioms " ident : command

@[command_elab harnessAxioms]
def elabHarnessAxioms : CommandElab
  | `(#harness_axioms $id:ident) => do
    let name ← liftCoreM <| realizeGlobalConstNoOverloadWithInfo id
    let axioms ← collectAxioms name
    let bad := axioms.filter (fun a => !allowedAxioms.contains a)
    if bad.isEmpty then
      logInfo m!"HARNESS_AXIOMS_OK {name}: {axioms.toList}"
    else
      logError m!"HARNESS_AXIOMS_BAD {name}: {bad.toList}"
  | _ => throwUnsupportedSyntax

end Harness
