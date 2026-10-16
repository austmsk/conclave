# 0007 — go.yaml.in/yaml/v3 for configuration

**Date:** 2026-10-16
**Status:** accepted

## Context

NFR-M7 requires configuration in YAML files, validated at startup with clear error messages. The standard library has no YAML parser, so reading `deploy/router.yaml` needs a third-party dependency, and NFR-M4 requires a record for every new one. The candidates:

- `gopkg.in/yaml.v3`, the long-standing go-yaml package. Its repository has been archived and is no longer maintained.
- `go.yaml.in/yaml/v3`, the continuation of that code under the YAML organisation, with the same API. It is maintained.
- `sigs.k8s.io/yaml`, which converts YAML to JSON and decodes with `encoding/json`. Structs then carry `json` tags for a YAML file, and errors refer to positions in the converted JSON rather than lines in the file the owner wrote. Recent versions also depend on `go.yaml.in/yaml` themselves.

## Decision

`internal/llm/config.go` imports `go.yaml.in/yaml/v3` directly, at v3.0.5. The decoder runs with `KnownFields(true)`, so a misspelt key is an error at startup instead of a setting that is silently ignored. That matters most for the router, where a misspelt fallback or price must not fall back to a default.

The module was already in the build graph at that version as an indirect dependency, pulled in by goose, testify and the OpenTelemetry modules. Using it directly moved it from the `// indirect` block of `go.mod` to the direct block and left `go.sum` unchanged. No new module enters the graph.

## Consequences

One more direct dependency to keep up to date. Its version is no longer decided only by whatever goose and OpenTelemetry require, but a bump now needs a deliberate check that `KnownFields` and the error messages still behave as the config tests expect.

YAML's loose typing remains a hazard: an unquoted value can parse as a different type than the author meant. Decoding into typed structs, followed by `Config.Validate`, is what keeps this in check, and every future configuration file (the planned repository-access file for FR-SEC12, among others) should go through the same decoder settings rather than a second YAML library.

I approved this dependency during ticket 4's review. This record is the NFR-M4 justification that should have come before it.
