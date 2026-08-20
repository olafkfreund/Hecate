"use client";

import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { Day } from "@/lib/api";

/**
 * What crossed and what failed, over the last fortnight.
 *
 * Two stacked areas rather than two lines: the question is "how much shipped,
 * and how much of it went wrong", and a stack answers both at once — the total
 * height is the throughput and the red band is the part that hurt.
 *
 * Failures are drawn on top so a single failure against a busy day is still
 * visible; underneath it would be a sliver at the baseline.
 */
export function ActivityChart({ days }: { days: Day[] }) {
  const total = days.reduce((n, d) => n + d.crossed + d.failed, 0);

  if (total === 0) {
    return (
      <p className="py-8 text-center text-sm text-[var(--muted-foreground)]">
        Nothing has crossed a Gate in the last {days.length} days.
      </p>
    );
  }

  return (
    <div className="h-48 w-full">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={days} margin={{ top: 4, right: 4, bottom: 0, left: -24 }}>
          <defs>
            <linearGradient id="crossedFill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--healthy)" stopOpacity={0.35} />
              <stop offset="100%" stopColor="var(--healthy)" stopOpacity={0.02} />
            </linearGradient>
            <linearGradient id="failedFill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--destructive)" stopOpacity={0.4} />
              <stop offset="100%" stopColor="var(--destructive)" stopOpacity={0.05} />
            </linearGradient>
          </defs>

          <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
          <XAxis
            dataKey="date"
            tickFormatter={short}
            tick={{ fontSize: 11, fill: "var(--muted-foreground)" }}
            stroke="var(--border)"
            interval="preserveStartEnd"
            minTickGap={24}
          />
          <YAxis
            allowDecimals={false}
            tick={{ fontSize: 11, fill: "var(--muted-foreground)" }}
            stroke="var(--border)"
            width={44}
          />
          <Tooltip
            contentStyle={{
              background: "var(--card)",
              border: "1px solid var(--border)",
              borderRadius: 8,
              fontSize: 12,
              color: "var(--foreground)",
            }}
            labelFormatter={(d: string) => long(d)}
          />
          <Area
            type="monotone"
            dataKey="crossed"
            name="crossed"
            stackId="1"
            stroke="var(--healthy)"
            fill="url(#crossedFill)"
            strokeWidth={2}
          />
          <Area
            type="monotone"
            dataKey="failed"
            name="failed"
            stackId="1"
            stroke="var(--destructive)"
            fill="url(#failedFill)"
            strokeWidth={2}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}

/** short is the axis label: "20 Aug", which fits fourteen of them. */
function short(iso: string): string {
  const d = new Date(iso + "T00:00:00Z");
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short", timeZone: "UTC" });
}

/** long is the tooltip label, where there is room to be unambiguous. */
function long(iso: string): string {
  const d = new Date(iso + "T00:00:00Z");
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(undefined, {
    weekday: "short",
    day: "numeric",
    month: "short",
    timeZone: "UTC",
  });
}
