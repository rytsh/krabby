// Shared formatting helpers.

// fmtDate renders timestamps as day.month.year hours:minutes (24h).
export function fmtDate(ts) {
  if (!ts || ts.startsWith("0001")) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "—";
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getDate())}.${p(d.getMonth() + 1)}.${d.getFullYear()} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
