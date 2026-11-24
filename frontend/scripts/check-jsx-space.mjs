// JSX text that starts with a space right after an element or expression and
// runs onto another line loses that space when compiled, so
// "<b>https</b> with a\n free cert" renders as "httpswith a free cert".
// Write the space explicitly instead: <b>https</b>{" "}with a ...
// Run by `npm run lint`; exits 1 on any match.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const here = path.dirname(fileURLToPath(import.meta.url));

function walk(dir, out = []) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else if (p.endsWith(".tsx")) out.push(p);
  }
  return out;
}

const problems = [];
for (const file of walk(path.join(here, "..", "src"))) {
  const sf = ts.createSourceFile(file, fs.readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const visit = (node) => {
    if (ts.isJsxText(node)) {
      const text = node.getFullText();
      const kids = node.parent && node.parent.children ? Array.from(node.parent.children) : [];
      const prev = kids[kids.indexOf(node) - 1];
      if (/^ +\S/.test(text) && text.includes("\n") && prev && !ts.isJsxText(prev)) {
        const { line } = sf.getLineAndCharacterOfPosition(node.getFullStart());
        problems.push(`${path.relative(process.cwd(), file)}:${line + 1}: leading space will be dropped; use {" "}`);
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
}

if (problems.length) {
  console.error(problems.join("\n"));
  process.exit(1);
}
