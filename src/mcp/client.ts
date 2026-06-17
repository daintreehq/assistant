/**
 * Daintree MCP client.
 *
 * Connects to Daintree's local MCP server over Streamable HTTP (falling back to
 * legacy SSE) with a bearer token. Designed for graceful degradation: if no
 * url/token is configured or the connection fails, the CLI keeps running in a
 * "degraded local mode" and tools that need Daintree report a clean error.
 *
 * Tests inject a `clientOverride` so no network is required.
 */
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { SSEClientTransport } from "@modelcontextprotocol/sdk/client/sse.js";
import type { AppConfig } from "../config.js";
import { DOCUMENTED_MCP_TOOL_NAMES } from "../models/prompts/daintreeMcp.js";

export interface McpToolInfo {
  name: string;
  description?: string;
  inputSchema: Record<string, unknown>;
}

export interface McpCallResult {
  /** Flattened text from all text content blocks. */
  text: string;
  content: unknown[];
  structuredContent?: unknown;
  isError: boolean;
}

/** The subset of the MCP SDK client we depend on (also what test fakes implement). */
export interface LowLevelMcpClient {
  listTools(): Promise<{ tools: Array<{ name: string; description?: string; inputSchema?: unknown }> }>;
  callTool(args: { name: string; arguments?: Record<string, unknown> }): Promise<{
    content?: unknown[];
    structuredContent?: unknown;
    isError?: boolean;
  }>;
  close?(): Promise<void>;
}

export interface McpClientOptions {
  /** Pre-built, already-connected low-level client (used in tests). */
  clientOverride?: LowLevelMcpClient;
}

export interface McpStatus {
  connected: boolean;
  url?: string;
  transport?: "streamable-http" | "sse" | "injected" | "none";
  toolCount?: number;
  error?: string;
  /**
   * Warnings about documented-vs-live tool drift, populated at startup. Each
   * entry names a tool we document (DOCUMENTED_MCP_TOOL_NAMES) that the live
   * server did not advertise. Undefined when there is no drift — drift never
   * fails the connection, it only surfaces here for the UI/doctor to render.
   */
  driftWarnings?: string[];
  /** The connected server's reported implementation info (name + version). */
  serverInfo?: { name?: string; version?: string };
}

export class DaintreeMcpClient {
  private cfg: AppConfig;
  private low?: LowLevelMcpClient;
  /**
   * The typed SDK client, kept alongside `low` so we can read server metadata
   * (getServerVersion / getServerCapabilities) that the LowLevelMcpClient
   * interface deliberately omits. Undefined when a clientOverride is injected.
   */
  private raw?: Client;
  private connected = false;
  private transportKind: McpStatus["transport"] = "none";
  private lastError?: string;
  private toolCache?: McpToolInfo[];
  private driftWarnings: string[] = [];
  private serverInfo?: { name?: string; version?: string };

  constructor(cfg: AppConfig, opts: McpClientOptions = {}) {
    this.cfg = cfg;
    if (opts.clientOverride) {
      this.low = opts.clientOverride;
      this.connected = true;
      this.transportKind = "injected";
    }
  }

  isConnected(): boolean {
    return this.connected;
  }

  status(): McpStatus {
    return {
      connected: this.connected,
      url: this.cfg.mcpUrl,
      transport: this.transportKind,
      toolCount: this.toolCache?.length,
      error: this.lastError,
      driftWarnings:
        this.driftWarnings.length > 0 ? [...this.driftWarnings] : undefined,
      serverInfo: this.serverInfo ? { ...this.serverInfo } : undefined,
    };
  }

  /** Attempt to connect. Never throws — returns whether it succeeded. */
  async connect(): Promise<McpStatus> {
    if (this.connected) {
      // An injected client is "connected" from construction but its cache was
      // never warmed — do it once here so toolCount and drift detection run.
      if (this.transportKind === "injected" && !this.toolCache) {
        await this.warmToolCache();
      }
      return this.status();
    }
    if (this.cfg.offline) {
      this.lastError = "offline mode";
      return this.status();
    }
    if (!this.cfg.mcpUrl || !this.cfg.mcpToken) {
      this.lastError = "DAINTREE_MCP_URL / DAINTREE_MCP_TOKEN not set";
      return this.status();
    }

    let url: URL;
    try {
      url = new URL(this.cfg.mcpUrl);
    } catch (e) {
      this.lastError = `invalid DAINTREE_MCP_URL: ${errMsg(e)}`;
      this.connected = false;
      return this.status();
    }
    const headers = { Authorization: `Bearer ${this.cfg.mcpToken}` };

    // Try Streamable HTTP first, then SSE.
    try {
      const client = new Client(
        { name: "daintree-assistant-cli", version: "0.1.0" },
        { capabilities: {} },
      );
      const transport = new StreamableHTTPClientTransport(url, {
        requestInit: { headers },
      });
      await client.connect(transport);
      this.raw = client;
      this.low = client as unknown as LowLevelMcpClient;
      this.connected = true;
      this.transportKind = "streamable-http";
      this.lastError = undefined;
      await this.warmToolCache();
      return this.status();
    } catch (httpErr) {
      // Fall back to SSE (legacy endpoint).
      try {
        const sseUrl = new URL(url.href);
        sseUrl.pathname = sseUrl.pathname.replace(/\/mcp\/?$/, "/sse");
        const client = new Client(
          { name: "daintree-assistant-cli", version: "0.1.0" },
          { capabilities: {} },
        );
        const transport = new SSEClientTransport(sseUrl, {
          requestInit: { headers },
        });
        await client.connect(transport);
        this.raw = client;
        this.low = client as unknown as LowLevelMcpClient;
        this.connected = true;
        this.transportKind = "sse";
        this.lastError = undefined;
        await this.warmToolCache();
        return this.status();
      } catch (sseErr) {
        this.lastError = `streamable-http: ${errMsg(httpErr)}; sse: ${errMsg(sseErr)}`;
        this.connected = false;
        return this.status();
      }
    }
  }

  /**
   * Drop any existing connection and attempt a fresh connect. Used by /doctor
   * and /reconnect so a CLI started before Daintree (or after a transport drop)
   * can recover without a full restart.
   */
  async reconnect(): Promise<McpStatus> {
    await this.close();
    this.connected = false;
    this.toolCache = undefined;
    this.transportKind = "none";
    this.lastError = undefined;
    this.driftWarnings = [];
    this.serverInfo = undefined;
    this.raw = undefined;
    return this.connect();
  }

  /** Populate the tool cache once after connecting so status shows a tool count. */
  private async warmToolCache(): Promise<void> {
    const before = { connected: this.connected, lastError: this.lastError };
    try {
      await this.listTools(true);
      // Drift is a best-effort, warning-only signal — runDriftCheck never throws,
      // but keep it inside the try so it only runs once we have a live tool list.
      this.runDriftCheck();
    } catch {
      // Best-effort: a transient tool-list failure must not flip a healthy
      // transport to "degraded" (listTools' catch calls markDegraded). The
      // connection stays up; the tool count is simply unknown.
      this.connected = before.connected;
      this.lastError = before.lastError;
    }
  }

  /**
   * Compare the documented tool surface against what the live server actually
   * advertises and record a warning for every documented tool that is missing.
   * Warning-only: never throws, never affects `connected`. We check missing-only
   * (documented names absent from the live set) — extra live tools are expected,
   * since the reference intentionally documents a verified subset, not the whole
   * surface.
   */
  private runDriftCheck(): void {
    try {
      this.driftWarnings = [];
      // Capture the server's reported implementation info if available. Isolated
      // so a metadata-fetch failure can never suppress the drift comparison below.
      try {
        const info = this.raw?.getServerVersion?.();
        this.serverInfo = info
          ? { name: info.name, version: info.version }
          : undefined;
      } catch {
        this.serverInfo = undefined;
      }
      const live = new Set((this.toolCache ?? []).map((t) => t.name));
      // No tools came back — treat as "unknown", not "everything drifted".
      if (live.size === 0) return;
      for (const name of DOCUMENTED_MCP_TOOL_NAMES) {
        if (!live.has(name)) {
          this.driftWarnings.push(
            `MCP drift: tool '${name}' is documented but missing from the live server`,
          );
        }
      }
    } catch {
      // Drift detection must never break startup.
      this.driftWarnings = [];
    }
  }

  private ensure(): LowLevelMcpClient {
    if (!this.connected || !this.low) {
      throw new McpUnavailableError(
        this.lastError ?? "Daintree MCP is not connected",
      );
    }
    return this.low;
  }

  /** Mark the connection degraded after a transport/protocol failure. */
  private markDegraded(e: unknown): void {
    this.connected = false;
    this.toolCache = undefined;
    this.driftWarnings = [];
    this.serverInfo = undefined;
    this.lastError = errMsg(e);
  }

  async listTools(force = false): Promise<McpToolInfo[]> {
    if (this.toolCache && !force) return this.toolCache;
    let res: Awaited<ReturnType<LowLevelMcpClient["listTools"]>>;
    try {
      res = await this.ensure().listTools();
    } catch (e) {
      if (!(e instanceof McpUnavailableError)) this.markDegraded(e);
      throw e;
    }
    this.toolCache = res.tools.map((t) => ({
      name: t.name,
      description: t.description,
      inputSchema: (t.inputSchema as Record<string, unknown>) ?? {
        type: "object",
        properties: {},
      },
    }));
    return this.toolCache;
  }

  async callTool(
    name: string,
    args: Record<string, unknown> = {},
  ): Promise<McpCallResult> {
    let res: Awaited<ReturnType<LowLevelMcpClient["callTool"]>>;
    try {
      res = await this.ensure().callTool({ name, arguments: args });
    } catch (e) {
      if (!(e instanceof McpUnavailableError)) this.markDegraded(e);
      throw e;
    }
    const content = (res.content as unknown[]) ?? [];
    const text = content
      .map((c) =>
        c && typeof c === "object" && "text" in c
          ? String((c as { text: unknown }).text)
          : "",
      )
      .filter(Boolean)
      .join("\n");
    return {
      text,
      content,
      structuredContent: res.structuredContent,
      isError: Boolean(res.isError),
    };
  }

  async close(): Promise<void> {
    try {
      await this.low?.close?.();
    } catch {
      /* ignore */
    }
    this.connected = false;
  }
}

export class McpUnavailableError extends Error {
  readonly code = "MCP_UNAVAILABLE";
  constructor(message: string) {
    super(message);
    this.name = "McpUnavailableError";
  }
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
