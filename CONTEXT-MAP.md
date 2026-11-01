# Context Map

## Contexts

- [Core](./CONTEXT.md) — serves, routes, and proxies HTTP; owns correctness, hardening, and extension points
- [Access policy](./extensions/policy/CONTEXT.md) — bundled extensions that identify callers and enforce rate limits and quotas

## Relationships

- **Access policy → Core**: Access policy uses only the core's public extension points. The core has no access-policy terms.
- **Access policy → Analytics**: Analytics records are interpretations of execution facts and HTTP observations. They are never a source for quota consumption.
