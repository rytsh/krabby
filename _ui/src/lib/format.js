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
