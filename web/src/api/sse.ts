import type { StreamEvent } from '../types'

export type SseStatus = 'connecting' | 'open' | 'closed'

export interface SseHandlers {
  onEvent: (ev: StreamEvent) => void
  onStatus?: (status: SseStatus) => void
}

// EventSourceClient wraps EventSource for one session with:
//  - automatic reconnect (EventSource's native retry with backoff),
//  - Last-Event-ID resume (the browser resends the last id; the server
//    replays only newer events),
//  - client-side seq dedupe as a belt-and-braces guard,
//  - synthetic reconnect markers so the UI can show connectivity.
export class EventSourceClient {
  private es: EventSource | null = null
  private lastSeq = 0
  private closedByUser = false
  private retryTimer: ReturnType<typeof setTimeout> | null = null
  private retryDelay = 1000

  constructor(
    private sessionId: string,
    private token: string,
    private handlers: SseHandlers,
  ) {}

  start(): void {
    this.closedByUser = false
    this.connect()
  }

  private connect(): void {
    if (this.closedByUser) return
    this.handlers.onStatus?.('connecting')
    // The token rides in the query: EventSource cannot set Authorization.
    const url = `/api/sessions/${encodeURIComponent(this.sessionId)}/events?token=${encodeURIComponent(this.token)}`
    const es = new EventSource(url)
    this.es = es

    es.onopen = () => {
      this.retryDelay = 1000
      this.handlers.onStatus?.('open')
    }

    es.onmessage = (msg: MessageEvent) => {
      // Heartbeats arrive as ": ping" comments and never reach onmessage.
      let ev: StreamEvent
      try {
        ev = JSON.parse(msg.data) as StreamEvent
      } catch {
        return
      }
      if (typeof ev.seq === 'number') {
        if (ev.seq <= this.lastSeq) return // replay/live overlap or dup
        this.lastSeq = Math.max(this.lastSeq, ev.seq)
      }
      // Track the browser-assigned last event id too.
      if (msg.lastEventId) {
        const n = Number(msg.lastEventId)
        if (Number.isFinite(n) && n > this.lastSeq) this.lastSeq = n
      }
      this.handlers.onEvent(ev)
    }

    es.onerror = () => {
      // EventSource auto-reconnects after the server's retry hint; if it
      // gives up (readyState CLOSED) we schedule our own retry with capped
      // exponential backoff.
      this.handlers.onStatus?.('connecting')
      if (es.readyState === EventSource.CLOSED) {
        this.scheduleReconnect()
      }
    }
  }

  private scheduleReconnect(): void {
    if (this.closedByUser) return
    if (this.retryTimer) return
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null
      this.retryDelay = Math.min(this.retryDelay * 1.5, 15_000)
      this.connect()
    }, this.retryDelay)
  }

  close(): void {
    this.closedByUser = true
    if (this.retryTimer) {
      clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
    this.es?.close()
    this.es = null
    this.handlers.onStatus?.('closed')
  }
}
