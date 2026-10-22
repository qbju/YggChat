// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import "time"

// In-memory rate limits run on the monotonic clock. With wall-clock Unix
// times, a clock set back (NTP fixing a phone that booted with a wrong date,
// a user changing the time) left every entry "in the future": the request
// limiter then never trimmed and, once full, refused all message requests,
// and catch-up answers to a member stopped, until the clock caught up again
// (hours or days).
var monoStart = time.Now()

// monoMs is milliseconds since start on the monotonic clock (a var for tests).
var monoMs = func() int64 { return time.Since(monoStart).Milliseconds() }
