# Repository Instructions

- Use the public Makefile targets described in
  [`DEVELOPMENT.md`](DEVELOPMENT.md) so local validation matches CI.
- Preserve every field defined by the Dev Container specification throughout
  parsing, merging, and resolution because repository configurations must
  remain portable across compliant Dev Container implementations.
- Define Kubernetes APIs in `internal/cluster/api` and run
  `make generate-kubernetes` so the canonical and Helm CRDs stay identical.
- Define internal RPC messages in their `.proto` sources and run
  `make proto-generate` so generated Go bindings remain reproducible.
- Write documentation for its audience: keep `README.md` focused on users, the
  Helm guide focused on cluster operators, and `DEVELOPMENT.md` focused on
  contributors. Explain the reason for an action where it affects correctness;
  keep protocol and controller rationale in the architecture documents.
- Run focused tests while iterating, then run `make lint`, `make test`, and
  `make test-frontend` before submitting a change.
