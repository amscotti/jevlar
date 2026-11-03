# ADR 0002: Fail-closed configuration with aggregated validation

- Status: Accepted
- Date: 2026-10-31

## Context

A gateway that boots half-configured — a listener bound, one provider
missing, a typo'd key name silently ignored — is worse than one that refuses
to start. Operators debug boot failures from terminal output, not dashboards.

## Decision

Configuration is fail-closed at boot: the file is read, `${VAR}` references
are expanded, the TOML is strictly decoded (unknown fields rejected), and
every semantic rule is validated — all errors aggregated into one report,
each anchored to a field path such as `providers[1].name`. Nothing binds a
socket until the report is empty; an invalid configuration exits with code 2
and leaves the port unbound. At runtime the polarity flips: a single bad
upstream never fails a request while another target is healthy, and a failed
reload keeps the previous configuration serving.

## Consequences

- Boot validation aggregates every violation; the operator sees all errors
  in one pass instead of fixing them one at a time.
- The exit-2-without-binding behaviour is asserted by an end-to-end test.
- Strict decoding means a configuration file for a newer Jevlar fails loudly
  on this binary rather than behaving differently.
- Key matching against the schema is case-sensitive: `[Server]` or
  `Config_Version` is reported as an unknown field even though go-toml's
  decoder case-folds key matching. Jevlar keeps the stricter behaviour —
  the canonical spelling is the only accepted one — so a key's casing is
  never silently forgiven on one path and rejected on another.
