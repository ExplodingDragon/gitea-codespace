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
- Keep `README.md` in a conventional GitHub project format with a concise
  overview, features, requirements, quick start, and links to detailed user,
  operator, and contributor documentation.
- Write documentation for its audience: keep `README.md` focused on users, the
  Helm guide focused on cluster operators, and `DEVELOPMENT.md` focused on
  contributors. Explain the reason for an action where it affects correctness;
  keep protocol and controller rationale in the architecture documents.
- Describe the current design positively and explain why a boundary or workflow
  exists. End each design section with implementation acceptance criteria that
  translate the design intent into behavior a contributor can verify.
- Keep detailed examples only when they help a reader complete the documented
  task. Link to the canonical guide instead of repeating the same procedure in
  several documents.
- Run focused tests while iterating, then run `make lint`, `make test`, and
  `make test-frontend` before submitting a change.
