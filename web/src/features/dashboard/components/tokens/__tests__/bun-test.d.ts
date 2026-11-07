// Type-only declaration for the subset of `bun:test` used by component tests
// running under `bun test` (node:test style). The package ships no types in
// this workspace; runtime resolution is provided by Bun itself.
declare module 'bun:test' {
  export const mock: {
    module: (specifier: string, factory: () => unknown) => void
  }
}
