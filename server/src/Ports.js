'use strict'

/**
 * Shared semantics for app port mappings, used by App (Docker publishing)
 * and Proxy (backend routing).
 *
 * A port entry is `{host, container, public?}`. `container` is always the port
 * the process listens on inside the container; `host` decides how it is exposed:
 *
 *   - `<number>` -> published on the host through Docker PortBindings.
 *   - `'proxy'`  -> not published; ODAC's reverse proxy routes to the container
 *                   port over the internal Docker network.
 *
 * `'proxy'` is the explicit spelling of what used to be an absent `host`.
 * Making it explicit lets the dashboard show who owns a port instead of
 * inferring ownership from a missing field.
 *
 * `'auto'` is an input-only value, resolved to a free host port before the
 * entry is persisted.
 *
 * The same container port may appear on several entries: the proxy reaches it
 * over the container network while Docker publishes it on the host, and the two
 * paths never meet.
 *
 * `public` only applies to published entries and decides the bind address:
 *
 *   - absent/false -> bound to 127.0.0.1. Reachable by the proxy and by other
 *                     processes on this host, by nothing else.
 *   - true         -> bound to every interface, so other machines can reach it.
 *
 * Publishing is opt-in per entry because a published port never traverses the
 * host firewall's INPUT chain: Docker's DNAT rewrites the destination in
 * nat/PREROUTING, so the routing decision forwards the packet to the container
 * and it only ever meets the FORWARD chain, where ufw has no rules. A public
 * entry is reachable from the internet even when ufw says otherwise, so it must
 * never become the default for an entry that did not ask for it.
 *
 * `auto` marks a container port ODAC guessed rather than one the user or a
 * recipe declared. Only a guess may be corrected by runtime auto-discovery.
 */
const PROXY = 'proxy'

/** Bind address for a published-but-not-public entry. */
const LOOPBACK = '127.0.0.1'

/**
 * Bind address for a public entry. Empty means "every interface" to Docker,
 * which resolves it to 0.0.0.0 plus [::] when the host has IPv6. Naming the
 * families explicitly instead would fail container create on IPv6-less hosts.
 */
const ANY = ''

class Ports {
  /** Sentinel `host` value marking an entry as reverse-proxy routed. */
  get PROXY() {
    return PROXY
  }

  /**
   * True when the entry is routed by the ODAC proxy rather than published.
   * A missing `host` counts as proxy so configs written before the sentinel
   * existed keep resolving correctly.
   *
   * @param {{host?: number|string, container?: number}} entry - Port mapping
   * @returns {boolean}
   */
  isProxy(entry) {
    return !!entry && (!entry.host || entry.host === PROXY)
  }

  /**
   * True when the entry must be handed to Docker as a host port binding.
   *
   * @param {{host?: number|string, container?: number}} entry - Port mapping
   * @returns {boolean}
   */
  isPublished(entry) {
    return !!entry && !this.isProxy(entry)
  }

  /**
   * True when the entry is published on every interface rather than loopback.
   * Proxy-routed entries are never public: they have no host binding at all.
   *
   * @param {{host?: number|string, public?: boolean}} entry - Port mapping
   * @returns {boolean}
   */
  isPublic(entry) {
    return this.isPublished(entry) && entry.public === true
  }

  /**
   * The `HostIp` to hand Docker for a published entry.
   *
   * @param {{host?: number|string, public?: boolean}} entry - Port mapping
   * @returns {string} '' for every interface, '127.0.0.1' otherwise
   */
  bindIp(entry) {
    return this.isPublic(entry) ? ANY : LOOPBACK
  }

  /**
   * True when ODAC guessed this entry's container port instead of being told it.
   * Only a guess may be rewritten by runtime auto-discovery.
   *
   * @param {{auto?: boolean}} entry - Port mapping
   * @returns {boolean}
   */
  isAuto(entry) {
    return !!entry && entry.auto === true
  }

  /**
   * The entry the reverse proxy routes, and the app's main container port.
   * Why not `ports[0]`: an app may publish a port and also sit behind the proxy,
   * and the dashboard does not guarantee an order between the two.
   *
   * Falls back to the first entry when nothing is proxy-routed, so an app that
   * only publishes still resolves a backend.
   *
   * @param {Array<{host?: number|string, container?: number}>} ports - Port mappings
   * @returns {object|undefined} The primary entry, or undefined when there are none
   */
  primary(ports) {
    if (!Array.isArray(ports) || ports.length === 0) return undefined

    return ports.find(entry => this.isProxy(entry)) || ports[0]
  }

  /**
   * Builds the entry for a container port ODAC inferred (image EXPOSE, or the
   * 3000 default). Centralized so a guess can never be persisted without its
   * `auto` marker, which is what keeps auto-discovery off declared ports.
   *
   * @param {number} containerPort - The inferred container port
   * @returns {{host: string, container: number, auto: boolean}}
   */
  discovered(containerPort) {
    return {host: PROXY, container: containerPort, auto: true}
  }

  /**
   * Coerces a `public` flag that may have crossed a JSON or form boundary.
   * Why: Hub sends port entries over a WebSocket and the dashboard may serialize
   * the checkbox as a string, so `'false'` must not read as truthy.
   *
   * @param {*} value - Raw flag from an untrusted payload
   * @returns {boolean|null} The flag, or null when the value is not a boolean
   */
  parsePublic(value) {
    if (value === undefined || value === null || value === '') return false
    if (typeof value === 'boolean') return value
    if (value === 'true') return true
    if (value === 'false') return false

    return null
  }

  /**
   * Rewrites legacy entries in place to the canonical shape.
   * Why: entries persisted before the sentinel omit `host` entirely, and both
   * the dashboard and `setPorts` validation now expect it to be present.
   *
   * A legacy proxy entry is also marked as a guess. No shipped surface let a
   * proxy container port be chosen by hand, so every one of them was inferred by
   * ODAC — and marking them keeps auto-discovery correcting them exactly as it
   * did before the marker existed. Only entries loaded from disk pass through
   * here, so a port the user sets from now on is never marked.
   *
   * @param {Array<{host?: number|string, container?: number}>} ports - Port mappings
   * @returns {Array} The same array, normalized in place
   */
  normalize(ports) {
    if (!Array.isArray(ports)) return ports

    for (const entry of ports) {
      if (entry && !entry.host) {
        entry.host = PROXY
        entry.auto = true
      }
    }

    return ports
  }
}

module.exports = new Ports()
