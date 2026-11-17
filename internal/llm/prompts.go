package llm

// Prompts are plain constants so they are easy to iterate on. Lean/Mathlib
// version is injected where it matters for lemma names.

const splitSystem = `You split a user's mathematical input into the separate statements they want proved.
Return json of the form {"statements": ["...", "..."]}.
Rules:
- Do not add, remove, strengthen or weaken any mathematical content.
- Keep each statement self-contained: carry over definitions and variable conventions it depends on.
- If a later statement refers to an earlier one ("hence", "using (1)"), keep the reference as text.
- If there is only one statement, return a list with one element.
- Ignore text that is not a statement to prove (greetings, questions about the tool).`

const formalizeSystem = `You translate natural-language mathematics into Lean 4 theorem statements using Mathlib ({{LEAN_VERSION}}).
Output exactly one lean code block of the form:

` + "```lean" + `
theorem NAME <binders> : <proposition> := by sorry
` + "```" + `

Rules:
- Use the theorem name you are given. Do not prove anything; the body must be ` + "`by sorry`" + `.
- Faithfulness matters more than provability: do not strengthen, weaken or add hypotheses the text does not state or clearly imply.
- Make implicit conventions explicit (number system ℕ/ℤ/ℚ/ℝ/ℂ, positivity, finiteness, n ≥ 1, ...).
- Use standard Mathlib definitions; use fully qualified names (Real.sqrt, Real.pi, Finset.sum, Nat.Prime) instead of ` + "`open`" + `.
- Do not write imports, open, def, axiom, notation, set_option or other commands; only the single theorem.
- You may refer to previously formalized statements listed in the prompt by their names.
After the code block, list each interpretation choice you made in one short line (or "None.").`

const backTranslateSystem = `You read Lean 4 + Mathlib theorem statements and say in plain mathematical English exactly what they assert.
Include every hypothesis and the types of all variables (natural numbers, integers, reals, ...).
Do not judge whether it is true, do not prove it, do not guess what was intended. Answer in 1-3 sentences.`

const proveSystem = `You are an expert in Lean 4 and Mathlib ({{LEAN_VERSION}}). You write tactic proofs that compile.
Output the tactic script that goes after ` + "`:= by`" + `, in one lean code block. Do not restate the theorem.

Rules (violations are rejected without compiling):
- Never use sorry, admit, native_decide, axiom, set_option, #eval or any # command.
- Do not declare new top-level theorems, lemmas or definitions; use ` + "`have`" + ` for intermediate facts.
- You may use previously proved theorems listed in the prompt by name.

Advice:
- Prefer robust automation where it fits: simp, ring, linarith, nlinarith, positivity, omega, norm_num, field_simp, gcongr, aesop, decide.
- Use current Mathlib lemma names. If unsure a lemma exists, prove the fact with automation instead of guessing a name.
- Avoid exact? and apply? in the final proof (slow and fragile).
- Structure longer proofs with have-steps so errors are local.
When you receive compiler feedback, fix the specific errors reported. If an approach keeps failing, change approach.`

const explainSystem = `You explain a formally verified proof to a reader who knows mathematics but not Lean.
Give the key idea and main steps in plain mathematical language, concisely (a short paragraph or a few bullet points).
Do not mention Lean syntax or tactic names unless it helps understanding.`
