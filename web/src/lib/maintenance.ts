// Shared wording for planned maintenance windows.
import type { MaintenanceWindow, Occurrence } from './api';

export const weekdayShort = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
/** Monday first, the way most calendars outside the US show a week. */
export const weekOrder = [1, 2, 3, 4, 5, 6, 0];

const dayTime = new Intl.DateTimeFormat(undefined, { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
const timeOnly = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });

export function fmtWhen(iso: string): string {
  return dayTime.format(new Date(iso));
}

/** fmtRange shows an occurrence, dropping the second date when it ends the same day. */
export function fmtRange(o: Occurrence): string {
  const s = new Date(o.start);
  const e = new Date(o.end);
  const sameDay = s.toDateString() === e.toDateString();
  return `${dayTime.format(s)} – ${sameDay ? timeOnly.format(e) : dayTime.format(e)}`;
}

export function fmtMinutes(min: number): string {
  if (min < 60) return `${min} min`;
  const h = Math.floor(min / 60);
  const m = min % 60;
  if (h >= 24 && m === 0 && h % 24 === 0) return `${h / 24} d`;
  return m ? `${h} h ${m} min` : `${h} h`;
}

export function scheduleText(w: MaintenanceWindow): string {
  if (w.kind === 'once') {
    return w.startsAt && w.endsAt ? fmtRange({ start: w.startsAt, end: w.endsAt }) : '';
  }
  const days = weekOrder.filter((d) => w.weekdays.includes(d)).map((d) => weekdayShort[d]);
  const every = days.length === 7 ? 'Every day' : `Every ${days.join(', ')}`;
  return `${every} at ${w.startTime} for ${fmtMinutes(w.duration ?? 0)} (${w.timeZone || 'hub time'})`;
}

/** stateText is the status of a window in words, never only a colour. */
export function stateText(w: MaintenanceWindow): string {
  switch (w.state) {
    case 'active':
      return w.current ? `Active until ${fmtWhen(w.current.end)}` : 'Active';
    case 'upcoming':
      return w.next ? `Upcoming · starts ${fmtWhen(w.next.start)}` : 'Upcoming';
    case 'disabled':
      return 'Disabled';
    default:
      return w.last ? `Past · ended ${fmtWhen(w.last.end)}` : 'Past';
  }
}

export const stateClass: Record<MaintenanceWindow['state'], string> = {
  active: 'maintenance',
  upcoming: '',
  past: '',
  disabled: '',
};

/** untilText is the short "until" of a pill. */
export function untilText(iso: string): string {
  const d = new Date(iso);
  const today = new Date().toDateString() === d.toDateString();
  return today ? timeOnly.format(d) : dayTime.format(d);
}
