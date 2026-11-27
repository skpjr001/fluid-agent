// Command fluid-agent is a forward proxy, webhook relay and callback ingest
// for IP-allowlisted servers.
package main

import "github.com/skpjr001/fluid-agent/internal/agent"

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() { agent.Main(version, lifecycle) }
