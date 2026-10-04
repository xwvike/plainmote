// Run by TestDiffModuleMatchesGo: reads cases as JSON on stdin, writes the
// diffs as JSON on stdout.
const { compare } = await import(process.env.DIFF_MODULE);
let input = "";
for await (const chunk of process.stdin) input += chunk;
const cases = JSON.parse(input);
process.stdout.write(JSON.stringify(cases.map((c) => compare(c.before, c.after, c.full))));
