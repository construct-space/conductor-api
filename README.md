# Conductor

Construct's always-on control plane for automations: claim and lease of scheduled work, desktop-preferred with the cloud operator as fallback. Go.

Part of [Construct](https://github.com/construct-space), the platform behind construct.space, published as it ran in September 2026. The organisation README maps the other services.

## Run

```
go run .
```

Copy `.env.sample` to `.env` and fill in the values; secrets are marked `change-me`.
A `Dockerfile` and a `captain-definition` are included: the service ran on CapRover.

## License

MIT, see `LICENSE`.