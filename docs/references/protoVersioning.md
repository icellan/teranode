# Protobuf Package Versioning Policy

Teranode's `.proto` files (the `*_api` service definitions under `services/*/*_api/`,
plus `pruner_api`) do not carry a version suffix in their `package` declaration today.
That's a deliberate, temporary state, not an oversight: every one of these packages
is still on its original wire format, so there has never been a breaking change to
version.

## The rule

- A package with no version suffix (`package blockchain_api;`) is implicitly **v1**.
  Nothing needs to change about it as long as every change to that `.proto` stays
  wire-compatible (new optional fields, new RPCs, new enum values appended at the
  end, etc.).
- A package only gains a version suffix (`package blockchain_api.v1;`, then
  `.v2`, ...) **at the moment a breaking change is introduced** — removing or
  renumbering a field, changing a field's type, removing an RPC, changing streaming
  semantics, or anything else that an old client/server pairing can't silently
  tolerate.
- Do not pre-emptively add a version suffix to a package that hasn't had a breaking
  change. Renaming a package churns every generated import across the service that
  owns it (and any service that imports it), for no behavioral benefit — that cost
  is paid once, when it's actually needed.

Each `.proto` file's `package` declaration carries a short comment recording this:

```proto
// Version: v1 (implicit). This package has no version suffix because it has
// never had a breaking change. When a breaking change is introduced, bump to
// package blockchain_api.v1 (or the next version) at that time -- not before.
package blockchain_api;
```

## When a breaking change lands

1. Rename the package (`blockchain_api` → `blockchain_api.v1`), update the comment
   above it, regenerate the Go code (`make gen`), and fix every import path the
   rename touches.
2. Decide whether the old and new versions need to run side by side during rollout.
   Teranode is a multi-node network with independently upgraded services, so a
   breaking wire-format change generally can't assume every peer/service upgrades
   atomically — see how `validator_txmeta_wireFormat` lets two Kafka tx-meta batch
   formats (`v1`/`v2`) coexist on the same topic during a rollout
   (`docs/references/kafkaMessageFormat.md`), and how the blockchain-service admin
   gRPC auth gate was rolled out method-by-method rather than as an atomic cutover.
   The same "old and new must coexist mid-rollout" concern applies to any versioned
   proto package: plan for a window where some nodes speak the old version and some
   speak the new one, and decide explicitly whether the server needs to accept both.
3. Only remove the old version's code path once the rollout window has closed and
   no node in the network can still be sending the old format.

## Scope

This policy covers the request/response wire contracts in `services/*/*_api/*.proto`
and `services/pruner/pruner_api/pruner_api.proto`. It does not apply to internal-only
`.proto` files that aren't a service's external API surface (e.g. `model/model.proto`,
`errors/error.proto`, `stores/utxo/status.proto`).
