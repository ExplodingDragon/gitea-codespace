# Repository Instructions

- Follow [`DEVELOPMENT.md`](DEVELOPMENT.md) for the supported build, generation,
  test, and validation workflows.
- Preserve every field defined by the Dev Container specification throughout
  parsing, merging, and resolution so standard configurations round-trip
  without losing data.
- Change Kubernetes APIs in `internal/cluster/api` and regenerate both CRD
  copies with `make generate-kubernetes`.
- Change internal RPC definitions in their `.proto` sources and regenerate the
  Go bindings with `make proto-generate`.
- Write documentation for its audience: keep `README.md` focused on users, the
  Helm guide focused on cluster operators, and `DEVELOPMENT.md` focused on
  contributors. Explain why a choice matters and the action the reader should
  take; keep protocol and controller rationale in the design documents.
- Keep documentation concise and current. Describe the supported workflow in
  positive terms and keep each implementation detail in the single document
  whose audience needs it.
- Run the focused tests for the changed package and `make lint` before
  submitting a change. Use `make test` and `make test-frontend` for the complete
  standard test suite.
