import React from "react";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  getSessionStatus,
  sessionEventsUrl,
  type MigrationEvent,
} from "@/lib/api";
import { cn } from "@/lib/utils";

const levelClasses: Record<MigrationEvent["level"], string> = {
  info: "text-foreground",
  progress: "text-muted-foreground",
  warn: "text-amber-500",
  error: "text-red-500",
};

type RunState = "running" | "succeeded" | "failed";

export default function LogSheet({
  open,
  setOpen,
  sessionId,
}: {
  open: boolean;
  setOpen: (open: boolean) => void;
  sessionId: string | null;
}) {
  const [events, setEvents] = React.useState<MigrationEvent[]>([]);
  const [runState, setRunState] = React.useState<RunState>("running");
  const scrollRef = React.useRef<HTMLDivElement>(null);

  React.useEffect(() => {
    if (!open || !sessionId) {
      return;
    }

    setEvents([]);
    setRunState("running");

    const source = new EventSource(sessionEventsUrl(sessionId));
    const seen = new Set<number>();

    source.onmessage = (message) => {
      const event = JSON.parse(message.data) as MigrationEvent;
      // Negative ids mark banners/synthetic events and may repeat; only
      // store-backed ids deduplicate across reconnects.
      if (event.id >= 0) {
        if (seen.has(event.id)) return;
        seen.add(event.id);
      }
      setEvents((current) => [...current, event]);

      // The stream stays open after a terminal event: a later resume (from
      // here or the CLI) appends to the same session and shows up live.
      if (event.message.startsWith("✅ Migration completed")) {
        setRunState("succeeded");
      } else if (event.message.startsWith("❌ Migration failed")) {
        setRunState("failed");
      } else if (
        event.message.startsWith("🚀 Starting") ||
        event.level === "progress"
      ) {
        setRunState("running");
      } else if (event.level === "error") {
        void getSessionStatus(sessionId)
          .then((status) => {
            if (status.status === "failed") {
              setRunState("failed");
            }
          })
          .catch(() => undefined);
      }
    };
    source.onerror = () => {
      // EventSource reconnects automatically with Last-Event-ID; nothing to
      // do here.
    };

    return () => {
      source.close();
    };
  }, [open, sessionId]);

  React.useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [events]);

  const title =
    runState === "succeeded"
      ? "Migration completed"
      : runState === "failed"
        ? "Migration failed"
        : "Migration in progress";

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetContent className="shadow-none flex w-full flex-col sm:max-w-2xl">
        <SheetHeader>
          <SheetTitle>{title}</SheetTitle>
          <SheetDescription>
            {sessionId ? (
              <>
                Session <code className="font-mono">{sessionId}</code> — resume
                later with{" "}
                <code className="font-mono">
                  pgstream session --id {sessionId}
                </code>
              </>
            ) : (
              "Waiting for session..."
            )}
          </SheetDescription>
        </SheetHeader>
        <div
          ref={scrollRef}
          className="mx-4 mb-4 flex-1 overflow-y-auto rounded-md border bg-muted/30 p-3 font-mono text-xs"
        >
          {events.length === 0 ? (
            <p className="text-muted-foreground">Waiting for events...</p>
          ) : (
            events.map((event) => (
              <p
                key={event.id}
                className={cn("whitespace-pre-wrap", levelClasses[event.level])}
              >
                {event.message}
              </p>
            ))
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}
