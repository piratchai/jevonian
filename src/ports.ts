import { connect } from "node:net";

/**
 * Whether something is already listening on `port` at `host`.
 *
 * Probes by connecting rather than binding, so it never reserves the port, and treats a
 * timeout as free so a firewalled-but-idle port is not mistaken for a running server. Used
 * before a foreground `serve` binds, so a local run refuses the port a running instance
 * already holds instead of fighting it (or appearing to replace it) for the socket.
 */
export function portInUse(port: number, host = "127.0.0.1", timeoutMs = 1_000): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = connect({ host, port });
    const finish = (result: boolean): void => {
      socket.removeAllListeners();
      socket.destroy();
      resolve(result);
    };
    socket.once("connect", () => finish(true));
    socket.once("error", () => finish(false));
    socket.setTimeout(timeoutMs, () => finish(false));
  });
}

/** The host to probe for a listen address: a wildcard bind still answers on loopback. */
export function probeHost(host: string): string {
  const trimmed = host.trim();
  return trimmed === "0.0.0.0" || trimmed === "::" || trimmed === "*" || trimmed === ""
    ? "127.0.0.1"
    : trimmed;
}
