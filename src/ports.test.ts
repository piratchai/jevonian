import { createServer, type AddressInfo } from "node:net";

import { describe, expect, it } from "vite-plus/test";

import { portInUse, probeHost, waitForPort } from "./ports";

describe("portInUse", () => {
  it("finds a listening port and a free one", async () => {
    const server = createServer();
    await new Promise<void>((resolve) => server.listen({ host: "127.0.0.1", port: 0 }, resolve));
    const address = server.address();
    const port = typeof address === "object" && address !== null ? address.port : 0;
    try {
      expect(await portInUse(port)).toBe(true);
    } finally {
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
    // The port is released now, so it reads free.
    expect(await portInUse(port)).toBe(false);
  });
});

describe("probeHost", () => {
  it("probes loopback for a wildcard bind and the host otherwise", () => {
    expect(probeHost("0.0.0.0")).toBe("127.0.0.1");
    expect(probeHost("::")).toBe("127.0.0.1");
    expect(probeHost("*")).toBe("127.0.0.1");
    expect(probeHost("")).toBe("127.0.0.1");
    expect(probeHost("127.0.0.1")).toBe("127.0.0.1");
    expect(probeHost("192.168.1.5")).toBe("192.168.1.5");
  });
});

describe("waitForPort", () => {
  it("resolves once the port accepts a connection", async () => {
    const server = createServer();
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const port = (server.address() as AddressInfo).port;
    try {
      // Not listening yet on a free high port — should time out quickly.
      expect(await waitForPort(port + 1, "127.0.0.1", 200, 40)).toBe(false);
      expect(await waitForPort(port, "127.0.0.1", 1_000, 40)).toBe(true);
    } finally {
      await new Promise<void>((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      );
    }
  });
});
