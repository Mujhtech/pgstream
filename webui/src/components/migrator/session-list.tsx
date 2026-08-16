import React from "react";
import { Button } from "@/components/ui/button";
import { listSessions, type SessionSummary } from "@/lib/api";
import { cn } from "@/lib/utils";

const statusClasses: Record<string, string> = {
  running: "text-blue-500",
  in_progress: "text-blue-500",
  done: "text-emerald-500",
  succeeded: "text-emerald-500",
  error: "text-red-500",
  failed: "text-red-500",
  created: "text-muted-foreground",
};

export default function SessionList({
  onWatch,
}: {
  onWatch: (id: string) => void;
}) {
  const [sessions, setSessions] = React.useState<SessionSummary[]>([]);
  const [loadError, setLoadError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let cancelled = false;
    const refresh = () => {
      listSessions()
        .then((rows) => {
          if (!cancelled) {
            setSessions(rows);
            setLoadError(null);
          }
        })
        .catch((error) => {
          if (!cancelled) {
            setLoadError(
              error instanceof Error ? error.message : "failed to load sessions",
            );
          }
        });
    };
    refresh();
    const timer = setInterval(refresh, 3000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, []);

  if (loadError) {
    return (
      <p className="p-3 text-sm text-muted-foreground">
        Could not load sessions: {loadError}
      </p>
    );
  }
  if (sessions.length === 0) {
    return (
      <p className="p-3 text-sm text-muted-foreground">
        No sessions yet. Sessions started here or from the CLI appear with
        live progress.
      </p>
    );
  }

  return (
    <div className="flex flex-col divide-y">
      {sessions.map((session) => {
        const percent =
          session.rows_total > 0
            ? Math.min(
                100,
                Math.round((session.rows_copied / session.rows_total) * 100),
              )
            : session.status === "done" || session.status === "succeeded"
              ? 100
              : 0;
        return (
          <div
            key={session.id}
            className="flex items-center gap-3 p-3 text-sm"
          >
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2">
                <code className="truncate font-mono text-xs">{session.id}</code>
                <span
                  className={cn(
                    "text-xs font-medium",
                    statusClasses[session.status] ?? "text-muted-foreground",
                  )}
                >
                  {session.status}
                </span>
                {session.external ? (
                  <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] uppercase text-muted-foreground">
                    cli
                  </span>
                ) : null}
              </div>
              <div className="mt-1 flex items-center gap-2">
                <div className="h-1.5 w-40 overflow-hidden rounded bg-muted">
                  <div
                    className="h-full rounded bg-primary transition-all"
                    style={{ width: `${percent}%` }}
                  />
                </div>
                <span className="text-xs text-muted-foreground">
                  {session.tables_done}/{session.tables_total} tables ·{" "}
                  {session.rows_copied.toLocaleString()} rows
                </span>
              </div>
              {session.error ? (
                <p className="mt-1 truncate text-xs text-red-500">
                  {session.error}
                </p>
              ) : null}
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => onWatch(session.id)}
            >
              Watch
            </Button>
          </div>
        );
      })}
    </div>
  );
}
