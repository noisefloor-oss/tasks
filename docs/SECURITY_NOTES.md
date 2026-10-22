# Security notes

The engine reads and writes only its own task state. The preferences file
is a closed schema: it can name a compiled-in adapter and a model, never a
path, executable, argument, environment value, or credential. CLI adapters
invoke fixed basenames resolved from the invoking user's `PATH`, refuse
executables contained by the project boundary, run without a shell, and
never copy child stderr or authentication output into diagnostics or
project files. Claim, run, and saga tokens are fencing values, not
authentication, and appear only in acquisition output. The umbrella host
and the worktree broker add an installation trust boundary; see
[BROKER.md](BROKER.md).

Report vulnerabilities privately via GitHub Security Advisories; see
[SECURITY.md](../SECURITY.md).
