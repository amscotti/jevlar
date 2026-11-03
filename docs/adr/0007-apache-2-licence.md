# ADR 0007: Apache-2.0 licence

- Status: Accepted
- Date: 2026-10-31

## Context

Jevlar may eventually be published openly. The licence must permit
self-hosting and commercial use, include an explicit patent grant, and be
recognised by standard corporate approval processes.

## Decision

Jevlar is licensed under the Apache License, Version 2.0. The full text is
in the repository root as `LICENSE`; the project is named in `NOTICE`.

## Consequences

- Contributions are accepted under the Apache-2.0 terms; the NOTICE file
  must be preserved in distributions.
- Dependencies (go-toml/v2 is MIT, client_golang is Apache-2.0) are
  compatible.
