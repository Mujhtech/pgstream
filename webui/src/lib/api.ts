export interface ConnectorPayload {
  host: string;
  port: number;
  user: string;
  password: string;
  database: string;
  schema?: string;
}

export interface SessionPayload {
  mysql: ConnectorPayload;
  postgres: ConnectorPayload;
  batch_size?: number;
}

export interface MigrationEvent {
  id: number;
  time: string;
  level: "info" | "warn" | "error" | "progress";
  message: string;
  table?: string;
  processed_rows?: number;
  total_rows?: number;
}

export interface TableRecord {
  table_name: string;
  status: string;
  last_offset: number;
  row_count: number;
  error_message: string;
}

export interface SessionStatus {
  id: string;
  status: "idle" | "running" | "succeeded" | "failed";
  error: string;
  tables: TableRecord[] | null;
}

async function parseError(response: Response): Promise<string> {
  try {
    const body = (await response.json()) as { error?: string };
    if (body.error) return body.error;
  } catch {
    // fall through to the generic message
  }
  return `request failed with status ${response.status}`;
}

export async function createSession(
  payload: SessionPayload,
): Promise<{ id: string }> {
  const response = await fetch("/api/sessions", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  if (!response.ok) {
    throw new Error(await parseError(response));
  }
  return (await response.json()) as { id: string };
}

export interface SessionSummary {
  id: string;
  created_at: string;
  status: string;
  error?: string;
  tables_total: number;
  tables_done: number;
  rows_copied: number;
  rows_total: number;
  external: boolean;
}

export async function listSessions(): Promise<SessionSummary[]> {
  const response = await fetch("/api/sessions");
  if (!response.ok) {
    throw new Error(await parseError(response));
  }
  const body = (await response.json()) as { sessions: SessionSummary[] | null };
  return body.sessions ?? [];
}

export async function getSessionStatus(id: string): Promise<SessionStatus> {
  const response = await fetch(`/api/sessions/${encodeURIComponent(id)}`);
  if (!response.ok) {
    throw new Error(await parseError(response));
  }
  return (await response.json()) as SessionStatus;
}

export function sessionEventsUrl(id: string): string {
  return `/api/sessions/${encodeURIComponent(id)}/events`;
}
