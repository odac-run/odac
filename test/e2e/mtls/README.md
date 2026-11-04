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
Each scenario prints `PASS`, `FAIL` or `SKIP` with one line of evidence, and the
run fails unless all of them pass.

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
  which verifies the proxy's HMAC itself; `images/hub`: stand-in for the
  Cloud; `images/client`: the visitor
