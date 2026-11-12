# Prior art: requirements to logic, and conflicts between requirements

VeriMed and SAT-LLM are summarized from the papers; the others from their documentation, so check them before citing numbers.

## Summary

Turning requirements into logic and asking a solver whether they can all hold is an established idea, and since 2024 the usual form is an LLM translating and an SMT solver checking. What we found nowhere else is the combination Stele aims at: free-form specs from real repositories (Spec Kit, Kiro, OpenSpec), hundreds of statements across documents, a vocabulary built by the tool, and a registry kept over time. Every published evaluation found uses at most 144 curated requirements and a vocabulary that is fixed in advance or small.

Three findings matter for Stele's design:
1. **Tools that work reliably constrain the input** (FRET, ASSERT, SpeAR) or **fix the vocabulary by hand** (VeriMed: 85 typed variables written by the authors). Free text to logic with a vocabulary the tool invents is the open problem, which is where Stele's losses are (dropped clauses, one thing named twice).
2. **Disagreement between formalizations is a usable signal.** VeriMed samples a requirement five times and compares the samples with the solver; where they differ, the source is ambiguous or the translation unstable. It flags a clause such as "all input lines" that one reading keeps and another drops.
3. **Precision on unmodified real specs is not reported anywhere.** SAT-LLM's precision of 1.00 is over 55 requirements built to contain conflicts; VeriMed reports detection of injected mutations only. Stele is aimed at that question: the conflicts in specs as they are written.

## VeriMed: "Neurosymbolic Auditing of Natural-Language Software Requirements"

Hall and Eiers, Stevens Institute of Technology, arXiv [2605.13817](https://arxiv.org/abs/2605.13817), May 2026.

- **Domain:** medical-device safety requirements (hemodialysis, 64 requirements from the ABZ 2016 case study; a PCA pump, 144, as a generalization check).
- **Logic:** SMT-LIB2, quantifier-free linear real arithmetic with Booleans. A requirement is `p ⇒ q` or a state predicate; temporal requirements are approximated by current-state conditions.
- **Vocabulary:** one schema of 85 typed variables plus domain constraints `C`, **written by hand** and given in every prompt, so all requirements share one model `M = C ∧ ⋀ρᵢ`. Building the schema automatically is listed as future work.
- **Checks (Z3):**
  - consistency: is `M` satisfiable? (unsat core names the conflict);
  - vacuity: can the trigger `p` occur under `C`?;
  - violatability: can `C ∧ p ∧ ¬q` hold?;
  - redundancy: do the other requirements already imply this one?;
  - scenario questions: does `M ∧ s` entail an answer, or give a counterexample?
- **Ambiguity:** five formalizations per requirement at temperature 1.0 (Claude Sonnet 4.6), compared pairwise by two-way SMT equivalence. More than one equivalence class flags the requirement; solver witnesses then drive a clarifying rewrite. One model family throughout.
- **Results:**
  - 12 of 64 requirements (18.8%) flagged as ambiguous before clarification, 0 after; pairwise agreement rose from 44% to 100%. The flag rate is not checked against human judgment.
  - 63 of 64 injected faults detected (98.4%). **All 64 mutated sets stayed satisfiable**: detection came from comparing each formalization with the original's, not from the consistency check.
  - Counterexample-guided repair of answers to scenario questions: 46% first attempt to 98.5% final.
  - The hemodialysis set had 0 real inconsistencies and 2 redundant requirements.
  - No precision on unmodified requirements, no cost or latency.
- **Limits stated by the authors:** infusion devices only; proxies for faithfulness, not faithfulness; small sets compared with industrial ones.

**For Stele:**
- The check list matches ours: conflict, vacuous rule (Stele's `vacuous` is their vacuity, a condition that can never hold), dead response. Two we do not report: redundancy, and violatability, a rule no situation can break. Violatability catches a translation that came out always true. It does not catch a dropped clause whose remaining rule can still be broken; only comparing formalizations finds that.
- Their consistency check found nothing on a real spec, and their fault detection relies on an equivalence oracle Stele does not have. Their result does not show that the formal path finds real conflicts on its own, which is why Stele aims it with a review.
- Sampling and comparing formalizations works with one model. At five samples per statement it multiplies translation cost by five; applied only to a review candidate's two to six statements it costs cents.

## SAT-LLM: "Translation Titans, Reasoning Challenges: Satisfiability-Aided Language Models for Detecting Conflicting Requirements"

Fazelnia, Mirakhorli (Hawaii), Bagheri (Nebraska), ASE 2024 NIER track, 5 pages, [doi 10.1145/3691620.3695302](https://doi.org/10.1145/3691620.3695302).

- **Pipeline:** GPT-4 (ChatGPT) translates each requirement into first-order logic following a "formalization protocol"; Z3 checks the set; MUS enumeration (Liffiton and Malik) names minimal conflicting subsets.
- **Formalization protocol** (four rules from observed translation errors):
  - **terminology consistency:** one term per meaning, negation for antonyms (allow/permit → `allow`, stop/prevent → `Not(allow)`);
  - **action consolidation:** a general action covers its kinds (`modify` covers add, remove, update) and both are stated;
  - **requirement decomposition:** "X and Y shall have access" becomes two statements;
  - **function streamlining:** the main verb as the predicate (`access(system, X)`, not `haveSystemAccess(X)`).
- **Data:** a Broker-system set from Fazelnia et al. (RE 2024): 45 requirements and 12 conflicting subsets, extended by the authors with 10 requirements forming 3 composite conflicts; 55 requirements and 15 conflict sets in all, 4 of them among three or more requirements. Seven conflict types (Composite-Negation, Forbid-Stop, Input-Output, Output-Output, Start-Forbid, Two Frequencies, Negation).
- **Results:** SAT-LLM found 80% of conflict sets (73% of pairs, 100% of composites), precision 1.00, recall 0.83, F1 0.91. GPT-4 asked directly: precision 0.85, recall 0.31, F1 0.46. Misses were Forbid-Stop and Output-Output conflicts.
- **Limits:** one small synthetic set built to contain conflicts; no held-out data; no cost.

**For Stele:**
- The protocol's four rules are the same problems Stele found and partly solved: terminology consistency is `normalize`'s synonym merge, decomposition is "every clause" in the prompt rules. That a 2024 paper on 55 requirements hit them supports the view that they are general, not corpus-specific.
- The LLM-alone baseline is weak there (recall 0.31), but that was GPT-4 in 2024 on a set built around composite conflicts; a current model's review is a stronger baseline and must be measured, not assumed weak.

## Tools for structured requirements

- **NASA FRET** (open source, NASA Ames). Requirements in FRETish, a restricted English (scope, condition, component, shall, timing, response), compiled to past- and future-time temporal logic and Lustre. Realizability checking (JKind, Kind 2) reports requirement sets no implementation can satisfy against every environment, which is the conflict question for reactive systems. A 2025 study (SpecVerify, arXiv [2507.04857](https://arxiv.org/abs/2507.04857)) notes that the translation stage of the FRET and CoCoSim pipeline still needs much manual work. No LLM front end to FRETish was found.
- **GE ASSERT** with SADL (a controlled English for ontologies): conflict, completeness and test generation for avionics requirements.
- **SpeAR** (Collins Aerospace with the US Air Force Research Laboratory): structured requirements with consistency and realizability analysis.
- **EARS** templates ("WHEN … THE SYSTEM SHALL …") are a writing convention, not a tool; Kiro specs use them.

The lesson for Stele is the intermediate form: these tools get exact translation by having the author write in a parseable shape. Stele could ask the LLM for such a shape (one obligation per clause) and parse it deterministically, so a dropped clause shows as a missing line a person can see.

## LLM-based products and other research

- **AWS Automated Reasoning checks** (Amazon Bedrock Guardrails, [concepts](https://docs.aws.eu/bedrock/latest/userguide/automated-reasoning-checks-concepts.html)). Extracts variables and rules from a natural-language policy document into formal logic, then checks an LLM's answers against it. Verdicts: VALID, INVALID, SATISFIABLE, IMPOSSIBLE, TRANSLATION_AMBIGUOUS. Policies have an editable draft and numbered immutable versions; a refinement engine proposes logic fixes from failing tests, each approved by a person ([blog](https://aws.amazon.com/blogs/machine-learning/automated-reasoning-policy-refinement-in-amazon-bedrock)). The "up to 99%" accuracy is stated for translations it judges unambiguous. Its purpose is to check chatbot answers, not to find conflicts within the document. The design is close to Stele's (extracted vocabulary, rules, solver verdicts, human approval, versions); TRANSLATION_AMBIGUOUS as a verdict is worth copying.
- **AWS Kiro Requirements Analysis** (already listed in [01](01-vision.md#prior-art-and-positioning)): LLM to logic and SMT checking of one spec.
- **Sentence-level translators:** nl2spec (CAV 2023), NL2TL, ARSENAL (SRI) to temporal logic; Endres et al. to program postconditions; SpecGen, AutoSpec to JML or ACSL from code. Each is evaluated per sentence, without a shared vocabulary across documents.
- **String requirements:** LLM-based satisfiability checking with generated SMT and Python checkers, arXiv [2506.16639](https://arxiv.org/abs/2506.16639).
- **Survey:** "A Short Survey on Formalising Software Requirements using LLMs", arXiv [2506.11874](https://arxiv.org/abs/2506.11874): LLMs are more reliable at focused assertions than at complete specifications; hybrid LLM and solver methods are the direction.
- **Hand-written formal languages:** Alloy (bounded checking, the small-scope hypothesis Stele's bounded collections rely on), TLA+, Event-B, Z, Attempto Controlled English.

## Spec tools for coding agents

Checked 2026-11-09 against each tool's public documentation and release notes. A "no" below means the documentation showed none, not that none exists; check again before citing.

| Tool | Contradiction detection | Scope of one check | Mechanism | Decisions kept | Licence |
| --- | --- | --- | --- | --- | --- |
| Kiro Requirements Analysis (IDE 0.12, May 2026) | Yes: conflicting requirements come back as two-option questions with scenarios | One spec's `requirements.md` | LLM to SMT-LIB, a solver | No record; the answer rewrites the file | Proprietary |
| Spec Kit `/speckit.analyze`, `/speckit.clarify` | One item of an inconsistency pass | One feature directory and the constitution | An LLM prompt | Clarifications are written into that spec | MIT |
| OpenSpec `validate` | No semantic check found | The structure of a change | Structural rules | None | MIT |
| Tessl | None found | One feature | Scripts and an LLM | None | Mixed |
| adrkit | Contradicting architecture decisions | Decision records in git | Deterministic lint, no solver | Yes, in git | Apache-2.0 |
| Qodo Rules | Conflicts among coding rules | Their platform | Not stated | On their platform | Proprietary |

Kiro also derives property-based tests from requirements, which Stele does not do.

**What Stele shares with them.** LLM-to-SMT contradiction detection with two-option questions is Kiro's too. Model-free gates and decisions kept in git exist elsewhere (adrkit).

**What was found nowhere else.**
- Contradictions checked by a solver across every spec in a repository, and across time: a requirement written today against one approved months ago in another document.
- A registry of approved readings in git that every later change is checked against, with the owner's decisions and what they taught.
- One checker that reads Spec Kit, OpenSpec, Kiro and plain Markdown, so a project is not tied to the tool whose checker it uses.
