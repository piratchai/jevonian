import { networkInterfaces } from "node:os";

import { LAN_PORT_OFFSET, type Config, type LanConfig } from "./config";

/**
 * IPv4 addresses a peer on the same network can reach this machine at.
 *
 * Only non-internal IPv4 addresses qualify: loopback is not reachable from another host, and
 * link-local IPv6 needs a zone id a user would not know to type. Interfaces are returned in
 * the order the OS reports them, deduplicated, so the first entry is a stable "most likely"
 * address to show in the console.
 */
export function lanIpv4Addresses(): string[] {
  const found: string[] = [];
  for (const addresses of Object.values(networkInterfaces())) {
    for (const address of addresses ?? []) {
      if (address.family !== "IPv4") continue;
      if (address.internal) continue;
      if (found.includes(address.address)) continue;
      found.push(address.address);
    }
  }
  return found;
}

/** True when a bind address accepts connections from other machines. */
export function isReachableFromLan(host: string): boolean {
  const trimmed = host.trim();
  return trimmed === "0.0.0.0" || trimmed === "::" || trimmed === "*";
}

/** The port the LAN surface listens on, defaulting clear of the tunnel's `listen.port + 1`. */
export function lanPort(config: Config): number {
  return config.lan.port ?? config.listen.port + LAN_PORT_OFFSET;
}

/** The bind host for the LAN surface; every interface unless a specific one was chosen. */
export function lanBindHost(lan: LanConfig): string {
  return lan.host ?? "0.0.0.0";
}

/**
 * The base URLs a peer should use to reach this instance's `/v1` surface. When the bind is a
 * specific address only that address is offered; when it is all interfaces, every LAN IPv4 is.
 * Empty when the machine has no non-loopback interface, so the console can say so rather than
 * print a URL that cannot work.
 */
export function lanBaseUrls(config: Config, addresses: string[] = lanIpv4Addresses()): string[] {
  const port = lanPort(config);
  const host = lanBindHost(config.lan);
  const hosts = isReachableFromLan(host) ? addresses : [host];
  return hosts.map((address) => `http://${address}:${port}/v1`);
}
