# Repository Instructions

- Follow [`DEVELOPMENT.md`](DEVELOPMENT.md) for the supported build, generation,
  test, and validation workflows.
- Keep every field defined by the Dev Container specification represented
  throughout parsing, merging, and resolution, including fields that the
  current runtime does not consume yet.
- Change Kubernetes APIs in `internal/cluster/api` and regenerate both CRD
  copies with `make generate-kubernetes`.
- Change internal RPC definitions in their `.proto` sources and regenerate the
  Go bindings with `make proto-generate`.
- Keep user-facing behavior documented in `README.md` or the Helm guide, and
  keep contributor workflows in `DEVELOPMENT.md`.
- Write documentation for its audience: keep `README.md` focused on users,
  the Helm guide focused on cluster operators, and `DEVELOPMENT.md` focused on
  contributors. Explain why a choice matters and the action the reader should
  take, and record protocol or controller rationale in the design docs.
- Keep documentation concise and current. Describe the supported workflow in
  positive terms and keep each implementation detail in the single document
  whose audience needs it.
- Run the focused tests for the changed package and `make lint` before
  submitting a change. Use `make test` and `make test-frontend` for the complete
  standard test suite.
