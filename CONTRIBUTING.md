# Contributing

Use Go 1.26. Run `make test vet check`; run race tests for concurrency changes and
Docker integration for runtime changes. Normal tests must not require credentials,
Redis or cloud infrastructure. Keep upstream fixtures and source notices intact.

Keep runtime orchestration in providers, external SDK/API calls in thirdparty,
and Agent execution in harness. New providers need strict configuration,
credential validation, cancellation and error tests. Do not expose SDK types in
the public runtime contract. Never commit private environment profiles or generated tokens.

Document behavior changes, data compatibility and checks performed. Open an issue
or describe a design before large protocol/format changes. Contributions to Go Harness
are provided under the [MIT License](LICENSE) unless stated otherwise.
