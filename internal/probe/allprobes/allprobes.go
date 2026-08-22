// Package allprobes blank-imports every built-in probe plugin so their init()s register with the
// probe registry. Both binaries — the hub (cmd/smoked) and the vantage agent (cmd/smoke-agent) —
// import this one list instead of maintaining their own, so the two can't drift apart: an NTP
// target assigned to a remote vantage was silently dropped as "unknown probe" precisely because the
// agent's hand-kept import list had omitted ntpprobe (CODE_REVIEW M2). Add a new probe here once.
package allprobes

import (
	_ "github.com/seitzbg/heliograph/internal/probe/dns"
	_ "github.com/seitzbg/heliograph/internal/probe/fping"
	_ "github.com/seitzbg/heliograph/internal/probe/httpprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/irttprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/ntpprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/pingprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/sshprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/tcpconnect"
)
