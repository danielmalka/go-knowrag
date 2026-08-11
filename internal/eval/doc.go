// Package eval measures retrieval recall and tenant isolation.
//
// Only the CLI-facing contract exists so far: the two entry points, the Outcome they answer with,
// and the sentinel they answer with until there is anything to measure. S10 builds the golden-set
// harness behind RunGolden and S11 the isolation suite behind RunIsolation; both keep Outcome as
// what crosses to cmd/cli, because that is what decides the process exit code.
package eval
