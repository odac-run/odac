'use strict'

/**
 * Shared semantics for app port mappings, used by App (Docker publishing)
 * and Proxy (backend routing).
 *
 * A port entry is `{host, container}`. `container` is always the port the
 * process listens on inside the container; `host` decides how it is exposed:
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
 */
const PROXY = 'proxy'

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
   * Rewrites legacy entries in place to the canonical shape.
   * Why: entries persisted before the sentinel omit `host` entirely, and both
   * the dashboard and `setPorts` validation now expect it to be present.
   *
   * @param {Array<{host?: number|string, container?: number}>} ports - Port mappings
   * @returns {Array} The same array, normalized in place
   */
  normalize(ports) {
    if (!Array.isArray(ports)) return ports

    for (const entry of ports) {
      if (entry && !entry.host) entry.host = PROXY
    }

    return ports
  }
}

module.exports = new Ports()
