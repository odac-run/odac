# mTLS end-to-end harness

Runs the client certificate features against real binaries in Docker: the
agent built from this tree, its proxy, an app behind it and a visitor with
curl and openssl.

```bash
test/e2e/mtls/run.sh             # every suite, then tear down
test/e2e/mtls/run.sh custom      # only the named suites
KEEP=1 test/e2e/mtls/run.sh      # leave the stack up to poke at it
test/e2e/mtls/run.sh --down      # remove what KEEP=1 left
```

Without settings it builds the checkout it lives in. `AGENT_SRC=<tree>` or
`AGENT_REPO=<repo> AGENT_REF=<ref>` (exported with `git archive`) build another.
The edge suite also needs odac-tunnel: `TUNNEL_SRC=<tree>` or
`TUNNEL_REPO=<odac.cc repo> TUNNEL_REF=<ref>`, else an `odac.cc` checkout next to
the agent repo; with none of them the suite reports SKIP.
Each scenario prints `PASS`, `FAIL` or `SKIP` with one line of evidence, and the
run fails unless all of them pass.

| Suite | Nodes | Covers |
|---|---|---|
| `custom` | edge-a | optional and required modes, the signed headers, scope, revocation closing a WebSocket, a repeated CSR |
| `appapi` | edge-a | an app token reaching only its own records; `set`, `scope` and `ca.import` never, `--all` included |
| `shared` | edge-a, edge-b | one CA imported on two nodes, `cert.import`, per-node revocation |
| `perhost` | edge-a, edge-b | `--issuer per-host`: the CA names each handshake gives (`openssl s_client`), no certificate request and HTTP/3 kept on a host no certificate opens, one host's certificate refused on another, import with the chain, a renewal signed by the other node's issuer opening both |
| `edge` | edge-a, edge-b, home | odac-tunnel fail-closed start, the hub's `cert.*` requests (MTLS_PLAN.md §6), owner checks, a tunnel opened by certificate, replication and a revocation queued for an offline edge |

`appapi` builds on `custom`'s record, so run them together.

## Isolation

- Compose project `mtls-e2e` on an `internal` network: no route out, no
  published ports. Images are built on the host first, the only step that
  downloads anything.
- Every ODAC node is its own `docker:dind`, so the apps it starts never meet
  the host engine. The node switches off the swap manager, which would
  otherwise add swap to the Docker VM.
- Images are tagged `mtls-e2e/*`. Teardown is `compose down -v` plus those
  tags, and the run ends by diffing containers, networks and volumes against
  what it found.
- The run's PKI (a test root, the TLS certificate every node and the hub
  serve, the edges' shared client CA) is made fresh by the `pki` step into a
  volume that teardown removes. The tunnel secret and token come from
  `openssl rand` and reach containers through the environment only.
- `tunnel.odac.run`, which the agent dials as a constant, is a network alias of
  edge-a, whose certificate chains to the run's root.
- The docker CLI runs with a private, credential-free config: a credential
  helper waiting on a locked keychain stalls a headless build forever.
  `E2E_USER_DOCKER_CONFIG=1` keeps yours.

## Leak check

The last step greps every log (compose, agent, proxy, dockerd, each app's
container, and the run's own output) for PEM armour, CSR fields, env dumps of
app secrets, and the actual values of the secrets, keys, CSRs and certificates
the run produced. It reports counts and file names, never the matches.

## Layout

- `run.sh`: build, start, suites, leak check, teardown
- `scenarios/<suite>.sh`: one function per scenario, `suite_<name>` runs them
- `lib/`: docker and probe helpers, the leak check
- `images/node`: dind plus the agent; `images/origin`: the app under test,
  which verifies the proxy's HMAC itself and calls the agent's local API with
  its own key (`origin api`); `images/hub`: stand-in for the Cloud, recipes on
  :8080 and the edge protocol on :8443, driven through `/ctl`;
  `images/tunnel`: odac-tunnel; `images/client`: the visitor and the PKI step
