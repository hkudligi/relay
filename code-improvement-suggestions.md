Perform a comprehensive reliability and observability upgrade of this repository.

  Goal: make rly capable of running long-lived agent tasks safely across provider failures, process restarts, quota exhaustion, cancellation, and partial progress.

  Work in stages and maintain progress in the repository’s task artifacts:

  1. Inspect the full architecture, existing tests, CLI behavior, agent adapters, routing, persistence, orchestration, and Freebuff protocol.
  2. Document the current execution lifecycle and identify all failure points.
  3. Design and implement durable checkpoints for:
     - task state
     - selected provider/model
     - retry attempts
     - provider errors
     - streamed progress
     - partial results
     - cancellation
     - resume decisions
  4. Improve routing so failed models are removed individually before falling back to another model or provider.
  5. Ensure provider errors preserve stderr, structured error payloads, exit codes, session IDs, and relevant trace paths.
  6. Enforce one active Freebuff session globally per user account.
  7. Add liveness heartbeats and useful progress summaries without flooding terminal output.
  8. Make task completion semantic:
     - mutation tasks require repository evidence
     - analysis/documentation tasks may complete with a response
     - failed or partial tasks must remain resumable
  9. Add CLI commands or output needed to inspect:
     - task history
     - current checkpoint
     - retry chain
     - provider/model attempts
     - Freebuff run directory
     - latest status and result
  10. Add unit, integration, failure-injection, restart, cancellation, and concurrency tests.
  11. Run formatting, build, vet, and the complete test suite.
  12. Update the architecture and semantic-layer documentation with the final design.

  Requirements:

  - Work incrementally and record meaningful progress after every stage.
  - Do not discard existing user changes.
  - Do not reset or overwrite unrelated files.
  - Do not commit or push changes.
  - Do not launch nested rly or Freebuff tasks.
  - If a provider fails, preserve the exact error and continue from the latest checkpoint.
  - If the task cannot be completed within the current run, write a detailed checkpoint describing completed work, remaining work, files changed, tests run, and the exact next action.
  - Before finishing, provide a concise final report with changed files, validation results, unresolved issues, and resume instructions.