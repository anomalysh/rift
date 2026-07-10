#!/usr/bin/env bun
// rift entrypoint: parse args, resolve configuration, run the tunnel agent,
// and translate outcomes into process exit codes (see constants.ts EXIT).

import { parseArgs, usageText } from "./args.ts";
import { TunnelClient } from "./client.ts";
import {
  ConfigError,
  configFilePath,
  loadConfigFile,
  type ResolvedConfig,
  resolveConfig,
  writeConfigValues,
} from "./config.ts";
import { EXIT, VERSION } from "./constants.ts";
import { renderCompletion, renderManPage } from "./docgen.ts";
import { createLogger } from "./logger.ts";
import { buildPolicy } from "./policy.ts";

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
    case "run":
      break;
  }

  const config = loadConfig(parsed.flags);
  const logger = createLogger(config.logLevel);

  // Compile the visitor-access policy (A2-A5) from the flags; bcrypt hashing is
  // async, so it happens here before the client connects.
  const built = await buildPolicy(parsed.flags);
  if ("error" in built) {
    fail(`rift: ${built.error}\nRun 'rift --help' for usage.`, EXIT.USAGE);
  }

  const clientOpts = {
    config,
    protocol: parsed.protocol,
    port: parsed.port,
    logger,
    ...(parsed.subdomain !== undefined ? { subdomain: parsed.subdomain } : {}),
    ...("policy" in built && built.policy !== undefined
      ? { policy: built.policy }
      : {}),
  };
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

await main();
