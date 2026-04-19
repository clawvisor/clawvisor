// Module resolver hook that rewrites `.js` imports to `.ts` when the `.ts`
// sibling exists. Lets Node's built-in TypeScript stripping run the plugin's
// NodeNext-style imports (`from "./x.js"`) directly without a build step.

export async function resolve(specifier, context, nextResolve) {
  try {
    return await nextResolve(specifier, context);
  } catch (err) {
    if (specifier.endsWith(".js") && (context.parentURL ?? "").startsWith("file:")) {
      return nextResolve(specifier.replace(/\.js$/, ".ts"), context);
    }
    throw err;
  }
}
