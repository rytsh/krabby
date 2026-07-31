// Shared formatting helpers.

// fmtEta renders a remaining-time estimate in seconds as a short, coarse label
// ("45s", "12m", "1h 20m"). Precision is deliberately dropped as the estimate
// grows: a long run's estimate is not accurate to the second, and showing it
// that way invites the reader to trust it more than it deserves.
export function fmtEta(seconds) {
  if (!seconds || seconds <= 0) return "";
  if (seconds < 60) return `${Math.round(seconds)}s`;

  const mins = Math.round(seconds / 60);
  if (mins < 60) return `${mins}m`;

  const hours = Math.floor(mins / 60);
  const rest = mins % 60;
  return rest ? `${hours}h ${rest}m` : `${hours}h`;
}

// fmtDate renders timestamps as day.month.year hours:minutes (24h).
export function fmtDate(ts) {
  if (!ts || ts.startsWith("0001")) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "—";
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getDate())}.${p(d.getMonth() + 1)}.${d.getFullYear()} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// fmtAgo renders a completed timestamp relative to now using stable, coarse
// units suitable for a frequently refreshed activity list.
export function fmtAgo(ts, now = Date.now()) {
  if (!ts || ts.startsWith("0001")) return "";
  const then = new Date(ts).getTime();
  if (Number.isNaN(then)) return "";

  const seconds = Math.max(0, Math.floor((now - then) / 1000));
  if (seconds < 10) return "just now";
  if (seconds < 60) return `${seconds}s ago`;

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;

  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;

  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;

  const months = Math.floor(days / 30);
  if (months < 12) return `${months}mo ago`;

  return `${Math.floor(months / 12)}y ago`;
}
