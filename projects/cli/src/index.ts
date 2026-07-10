#!/usr/bin/env bun
// rift entrypoint: parse args, resolve configuration, run the tunnel agent,
// and translate outcomes into process exit codes (see constants.ts EXIT).

import { type FlagConfig, parseArgs, usageText } from "./args.ts";
import { TunnelClient } from "./client.ts";
import {
  ConfigError,
  configFilePath,
  loadConfigFile,
  type ResolvedConfig,
  resolveConfig,
  writeConfigValues,
} from "./config.ts";
import { EXIT, type SupportedProtocol, VERSION } from "./constants.ts";
import { renderCompletion, renderManPage } from "./docgen.ts";
import { createLogger, createNamedLogger, type Logger } from "./logger.ts";
import { buildPolicy } from "./policy.ts";
import { loadProjectConfig, selectTunnels, tunnelToArgv } from "./project.ts";
import { buildTrafficPolicy, TrafficController } from "./traffic.ts";

/** A single runnable tunnel: its protocol/port/subdomain and resolved flags. */
interface RunSpec {
  protocol: SupportedProtocol;
  port: number;
  subdomain?: string;
  flags: FlagConfig;
}

/**
 * Build the TunnelClient constructor options for one spec: resolve config,
 * compile the visitor-access policy (A2-A5) and traffic policy (T1-T6), and
 * attach custom domains (E1). Exits the process on any policy/config error.
 */
async function clientOptionsFor(
  spec: RunSpec,
  config: ResolvedConfig,
  logger: Logger,
): Promise<ConstructorParameters<typeof TunnelClient>[0]> {
  const built = await buildPolicy(spec.flags);
  if ("error" in built) {
    fail(`rift: ${built.error}\nRun 'rift --help' for usage.`, EXIT.USAGE);
  }
  const traffic = buildTrafficPolicy(spec.flags);
  if ("error" in traffic) {
    fail(`rift: ${traffic.error}\nRun 'rift --help' for usage.`, EXIT.USAGE);
  }
  return {
    config,
    protocol: spec.protocol,
    port: spec.port,
    logger,
    ...(spec.subdomain !== undefined ? { subdomain: spec.subdomain } : {}),
    ...("policy" in built && built.policy !== undefined
      ? { policy: built.policy }
      : {}),
    ...("policy" in traffic && traffic.policy !== undefined
      ? { traffic: new TrafficController(traffic.policy) }
      : {}),
    ...(spec.flags.domain !== undefined && spec.flags.domain.length > 0
      ? { domains: spec.flags.domain }
      : {}),
  };
}

function fail(message: string, code: number): never {
  process.stderr.write(message.endsWith("\n") ? message : `${message}\n`);
  process.exit(code);
}

function loadConfig(
  flags: Parameters<typeof resolveConfig>[0]["flags"],
): ResolvedConfig {
  const env = process.env;
  const configPath = configFilePath(env);
  try {
    const file = loadConfigFile(env);
    return resolveConfig({ flags, env, file, configPath });
  } catch (err) {
    if (err instanceof ConfigError) {
      fail(err.message, EXIT.ERROR);
    }
    throw err;
  }
}

async function main(): Promise<void> {
  const parsed = parseArgs(process.argv.slice(2));

  switch (parsed.kind) {
    case "help":
      process.stdout.write(`${usageText()}\n`);
      process.exit(EXIT.OK);
      break;
    case "version":
      process.stdout.write(`${VERSION}\n`);
      process.exit(EXIT.OK);
      break;
    case "error":
      fail(`rift: ${parsed.message}\nRun 'rift --help' for usage.`, EXIT.USAGE);
      break;
    case "man":
      process.stdout.write(renderManPage());
      process.exit(EXIT.OK);
      break;
    case "completions":
      process.stdout.write(renderCompletion(parsed.shell));
      process.exit(EXIT.OK);
      break;
    case "set-config": {
      try {
        const { path, keys } = writeConfigValues(process.env, parsed.updates);
        // Never echo the values themselves; the token is a secret.
        process.stdout.write(`rift: saved ${keys.join(", ")} to ${path}\n`);
        process.exit(EXIT.OK);
      } catch (err) {
        if (err instanceof ConfigError) {
          fail(err.message, EXIT.ERROR);
        }
        throw err;
      }
      break;
    }
    case "start":
      await runStart(parsed.names);
      return;
    case "run":
      break;
  }

  const config = loadConfig(parsed.flags);
  const logger = createLogger(config.logLevel);
  const clientOpts = await clientOptionsFor(parsed, config, logger);
  const client = new TunnelClient(clientOpts);

  let signalExit: number | null = null;
  const onSignal = (name: string, code: number): void => {
    if (signalExit !== null) {
      // A second signal forces an immediate exit.
      logger.close?.();
      process.exit(code);
    }
    signalExit = code;
    logger.status?.("closing");
    logger.info(`received ${name}, shutting down`);
    client.stop();
  };
  process.on("SIGINT", () => onSignal("SIGINT", EXIT.SIGINT));
  process.on("SIGTERM", () => onSignal("SIGTERM", EXIT.SIGTERM));

  try {
    await client.run();
    // Restore the terminal (show cursor, freeze the final panel) before exit.
    logger.close?.();
    process.exit(signalExit ?? EXIT.OK);
  } catch (err) {
    logger.error(err instanceof Error ? err.message : String(err));
    logger.close?.();
    process.exit(EXIT.ERROR);
  }
}

/**
 * `rift start [name...]`: open the named tunnels from the project config (D3).
 * Each runs its own TunnelClient concurrently with a name-tagged plain logger,
 * since the interactive dashboard cannot multiplex several tunnels.
 */
async function runStart(names: string[]): Promise<void> {
  const project = loadProjectConfig(process.cwd());
  if (project === null) {
    fail(
      "rift: no rift.yml (or .yaml/.toml/.json) found in this directory.",
      EXIT.USAGE,
    );
  }
  const selection = selectTunnels(project, names);
  if ("error" in selection) {
    fail(`rift: ${selection.error}`, EXIT.USAGE);
  }

  const clients = await Promise.all(
    selection.names.map(async (name) => {
      const translated = tunnelToArgv(name, project.tunnels[name]);
      if ("error" in translated) {
        fail(`rift: ${translated.error}`, EXIT.USAGE);
      }
      const spec = parseArgs(translated.argv);
      if (spec.kind === "error") {
        fail(`rift: tunnel "${name}": ${spec.message}`, EXIT.USAGE);
      }
      if (spec.kind !== "run") {
        fail(`rift: tunnel "${name}" is not a runnable tunnel`, EXIT.USAGE);
      }
      const config = loadConfig(spec.flags);
      const logger = createNamedLogger(name, config.logLevel);
      const opts = await clientOptionsFor(spec, config, logger);
      return { name, logger, client: new TunnelClient(opts) };
    }),
  );

  let signalExit: number | null = null;
  const onSignal = (sig: string, code: number): void => {
    if (signalExit !== null) {
      process.exit(code);
    }
    signalExit = code;
    for (const c of clients) {
      c.logger.info(`received ${sig}, shutting down`);
      c.client.stop();
    }
  };
  process.on("SIGINT", () => onSignal("SIGINT", EXIT.SIGINT));
  process.on("SIGTERM", () => onSignal("SIGTERM", EXIT.SIGTERM));

  const results = await Promise.allSettled(
    clients.map((c) =>
      c.client.run().catch((err: unknown) => {
        c.logger.error(err instanceof Error ? err.message : String(err));
        throw err;
      }),
    ),
  );
  const failed = results.some((r) => r.status === "rejected");
  process.exit(failed ? EXIT.ERROR : (signalExit ?? EXIT.OK));
}

await main();
