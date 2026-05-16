import { readdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const outDir = path.resolve(scriptDir, "../out");
const buildId = encodeURIComponent(process.env.WEBSHELL_BUILD_ID || String(Date.now()));

const staticAssetPattern =
  /(["'])((?:\/_next\/static\/|static\/)[^"'?]+\.(?:css|js))(?:\?v=[^"']*)?\1/g;
const escapedStaticAssetPattern =
  /(\\")((?:\/_next\/static\/|static\/)[^"\\?]+\.(?:css|js))(?:\?v=[^"\\]*)?(\\")/g;

for (const file of await htmlFiles(outDir)) {
  const source = await readFile(file, "utf8");
  const next = source
    .replace(staticAssetPattern, `$1$2?v=${buildId}$1`)
    .replace(escapedStaticAssetPattern, `$1$2?v=${buildId}$3`);
  if (next !== source) {
    await writeFile(file, next);
  }
}

async function htmlFiles(root) {
  const entries = await readdir(root, { withFileTypes: true });
  const files = [];

  for (const entry of entries) {
    const fullPath = path.join(root, entry.name);
    if (entry.isDirectory()) {
      files.push(...(await htmlFiles(fullPath)));
    } else if (entry.isFile() && entry.name.endsWith(".html")) {
      files.push(fullPath);
    }
  }

  return files;
}
