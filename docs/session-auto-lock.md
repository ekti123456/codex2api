# Session error auto-lock

`/session-errors` exposes a disabled-by-default policy with a default threshold of
3 (configurable from 1 to 10000). Only final HTTP 500 results with the explicit error code
`server_is_overloaded` count. SSE and
WebSocket requests use their terminal usage/error status, not their successful
HTTP handshake. Internal attempts do not count separately. Results are ordered
by completion within the existing user/platform/root-session identity; any
other result resets the streak, including handshake timeouts, DNS/TLS failures,
connection errors, unclassified HTTP 500 responses and other internal errors. Requests without a reliable session identity
cannot be locked by this feature.

Main requests and related background requests (including Guardian) share this
counter when they resolve to the same root. Once a final usage result is recorded,
later connection cancellation does not replace it. An actual final 499, or a
cancellation without a final usage result, still resets the streak.

API relay requests are excluded, including a relay selected from a mixed account
pool. An API key used to call a Codex account does not itself grant this exemption.

At the threshold the existing persistent session blacklist is updated with
`lock_source=automatic`. The UI labels it “自动锁定” and supports
`lock_state=auto_locked` in both statistics and blacklist views. Existing parent
and fork enforcement and exemptions remain in effect. In-flight requests are not
forcibly canceled. Administrators can unlock automatic entries normally.

Settings, streaks, and locks survive restart. Saving settings resets pending
streaks and invalidates older requests' policy snapshots. Disabling the policy
does not unlock existing entries. Manual unlock resets the streak and older
in-flight requests cannot immediately re-lock it. Historical errors are never
backfilled into streaks. Existing manual entries default to `lock_source=manual`.

Settings API (admin authentication required): GET / PUT
`/api/admin/session-errors/auto-lock` with `{ "enabled": false, "threshold": 3 }`.
The disabled path performs no counter database work; the enabled path uses
indexed per-session updates instead of scanning logs. Lock attribution queries
are limited to the current page. Persistence failure is logged and never changes
the already completed model response.

Final error classification prefers terminal usage, then the final HTTP response,
then the error observer. Missing codes in final usage never borrow an overload
code from an earlier internal retry. HTTP 500 alone is not proof of upstream
overload. Other final 500 errors remain in statistics while auto-lock is enabled.
`diagnostics.auto_lock_eligible` describes the error classification only; it does
not mean the setting was enabled, a counter was incremented, or a lock occurred.
The list's `count` remains cumulative errors, not the current streak.

The narrowed policy uses `session_overload_streaks` rather than inheriting legacy
`session_500_streaks` counters. Existing settings and locks remain unchanged.
Historical automatic locks require administrator review and manual unlock:
the latest error alone cannot reliably establish which results caused a lock.
