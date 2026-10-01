import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { DEFAULT_LAN, LAN_PORT_OFFSET, parseConfig, parseLan } from "./config";
import { isReachableFromLan, lanBaseUrls, lanBindHost, lanIpv4Addresses, lanPort } from "./lan";

const interfaces = vi.hoisted(() => ({ value: {} as NodeJS.Dict<unknown> }));

vi.mock("node:os", async (importOriginal) => {
  const actual = await importOriginal<typeof import("node:os")>();
  return {
    ...actual,
    networkInterfaces: () => interfaces.value,
  };
});

// parseConfig lets JEVONIAN_PORT override the config's listen.port, so a shell that exports it
// (a dev instance on a scratch port) would shift the port math these tests assert.
const previousPort = process.env.JEVONIAN_PORT;

beforeEach(() => {
  delete process.env.JEVONIAN_PORT;
});

afterEach(() => {
  interfaces.value = {};
  if (previousPort === undefined) delete process.env.JEVONIAN_PORT;
  else process.env.JEVONIAN_PORT = previousPort;
});

function configWith(host: string, port: number, lan: unknown = { enabled: true }) {
  return parseConfig({
    listen: { host, port },
    lan,
    providers: [],
  });
}

describe("parseLan", () => {
  it("is off unless explicitly enabled", () => {
    expect(parseLan(undefined)).toEqual(DEFAULT_LAN);
    expect(parseLan({})).toEqual(DEFAULT_LAN);
    expect(parseLan({ enabled: "yes" })).toEqual(DEFAULT_LAN);
    expect(parseLan({ enabled: true })).toEqual({ enabled: true });
  });

  it("keeps a valid host and port, dropping junk", () => {
    expect(parseLan({ enabled: true, host: "192.168.1.5", port: 9000 })).toEqual({
      enabled: true,
      host: "192.168.1.5",
      port: 9000,
    });
    expect(parseLan({ enabled: true, host: "  ", port: -1 })).toEqual({ enabled: true });
    expect(parseLan({ enabled: true, port: 1.5 })).toEqual({ enabled: true });
  });

  it("defaults to off in a parsed config", () => {
    expect(parseConfig({ providers: [] }).lan).toEqual(DEFAULT_LAN);
  });
});

describe("lanIpv4Addresses", () => {
  it("returns non-internal IPv4 only, deduplicated", () => {
    interfaces.value = {
      lo0: [
        { family: "IPv4", address: "127.0.0.1", internal: true, netmask: "", mac: "", cidr: null },
      ],
      en0: [
        {
          family: "IPv4",
          address: "192.168.1.20",
          internal: false,
          netmask: "",
          mac: "",
          cidr: null,
        },
        {
          family: "IPv6",
          address: "fe80::1",
          internal: false,
          netmask: "",
          mac: "",
          cidr: null,
          scopeid: 1,
        },
      ],
      en1: [
        {
          family: "IPv4",
          address: "192.168.1.20",
          internal: false,
          netmask: "",
          mac: "",
          cidr: null,
        },
        { family: "IPv4", address: "10.0.0.4", internal: false, netmask: "", mac: "", cidr: null },
      ],
    };
    expect(lanIpv4Addresses()).toEqual(["192.168.1.20", "10.0.0.4"]);
  });

  it("is empty when the machine has only loopback", () => {
    interfaces.value = {
      lo0: [
        { family: "IPv4", address: "127.0.0.1", internal: true, netmask: "", mac: "", cidr: null },
      ],
    };
    expect(lanIpv4Addresses()).toEqual([]);
  });
});

describe("lan binding", () => {
  it("treats wildcard hosts as reachable from the network", () => {
    expect(isReachableFromLan("0.0.0.0")).toBe(true);
    expect(isReachableFromLan("::")).toBe(true);
    expect(isReachableFromLan("*")).toBe(true);
    expect(isReachableFromLan("192.168.1.5")).toBe(false);
    expect(isReachableFromLan("127.0.0.1")).toBe(false);
  });

  it("defaults the port clear of the tunnel's listen.port + 1", () => {
    expect(LAN_PORT_OFFSET).toBe(2);
    expect(lanPort(configWith("127.0.0.1", 8787))).toBe(8789);
    expect(lanPort(configWith("127.0.0.1", 8787, { enabled: true, port: 9500 }))).toBe(9500);
  });

  it("binds every interface unless one was named", () => {
    expect(lanBindHost({ enabled: true })).toBe("0.0.0.0");
    expect(lanBindHost({ enabled: true, host: "10.0.0.9" })).toBe("10.0.0.9");
  });

  it("offers every LAN address when bound to all interfaces", () => {
    const config = configWith("127.0.0.1", 8787);
    expect(lanBaseUrls(config, ["192.168.1.20", "10.0.0.4"])).toEqual([
      "http://192.168.1.20:8789/v1",
      "http://10.0.0.4:8789/v1",
    ]);
  });

  it("offers only the named address when one was bound", () => {
    const config = configWith("127.0.0.1", 8787, { enabled: true, host: "10.0.0.9" });
    expect(lanBaseUrls(config, ["192.168.1.20"])).toEqual(["http://10.0.0.9:8789/v1"]);
  });

  it("returns nothing usable when there is no LAN address", () => {
    const config = configWith("127.0.0.1", 8787);
    expect(lanBaseUrls(config, [])).toEqual([]);
  });
});
