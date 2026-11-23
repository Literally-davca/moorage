// Readable labels for audit log action keys. The keys are stable (they live in the database),
// so this map is the only place that turns them into text.
const labels: Record<string, string> = {
  'stack.create': 'Created stack',
  'stack.save': 'Saved stack',
  'stack.delete': 'Deleted stack',
  'stack.up': 'Deployed stack',
  'stack.down': 'Took stack down',
  'stack.stop': 'Stopped stack',
  'stack.restart': 'Restarted stack',
  'stack.pull': 'Pulled images',
  'stack.update': 'Updated stack',
  'stack.restore': 'Restored a version',
  'image.policy': 'Changed update policy',
  'image.check': 'Checked images',
  'image.update': 'Updated images',
  'image.change_tag': 'Changed image tag',
  'image.rollback': 'Rolled back images',
  'image.auto_update': 'Auto-updated images',
  'updates.settings': 'Saved update settings',
  'updates.check': 'Checked all images',
  'host.add': 'Created join token',
  'host.paired': 'Paired agent',
  'host.rename': 'Renamed host',
  'host.remove': 'Removed host',
  'check.create': 'Added check',
  'check.save': 'Saved check',
  'check.delete': 'Deleted check',
  'channel.create': 'Added notification channel',
  'channel.save': 'Saved notification channel',
  'channel.delete': 'Deleted notification channel',
  'channel.test': 'Sent test notification',
  'watchdog.save': 'Saved hub watchdog',
  'watchdog.maintenance': 'Set hub maintenance',
  'watchdog.test': 'Tested hub watchdog',
  'statuspage.create': 'Added status page',
  'statuspage.save': 'Saved status page',
  'statuspage.delete': 'Deleted status page',
  'maintenance.create': 'Planned maintenance',
  'maintenance.save': 'Saved maintenance window',
  'maintenance.delete': 'Deleted maintenance window',
  'template_source.create': 'Added template index',
  'template_source.save': 'Saved template index',
  'template_source.delete': 'Removed template index',
  'template_source.refresh': 'Refreshed template index',
  'terminal.open': 'Opened terminal',
  'auth.login': 'Signed in',
  'auth.login_failed': 'Failed sign-in',
  'auth.logout': 'Signed out',
  'auth.setup': 'Finished setup',
};

export function auditLabel(action: string): string {
  return labels[action] ?? action;
}

/** Filter groups: the value is the action prefix the API filters by. */
export const auditGroups: { prefix: string; name: string }[] = [
  { prefix: 'stack.', name: 'Stacks' },
  { prefix: 'image.', name: 'Images' },
  { prefix: 'updates.', name: 'Update settings' },
  { prefix: 'terminal.', name: 'Terminal' },
  { prefix: 'host.', name: 'Hosts' },
  { prefix: 'check.', name: 'Checks' },
  { prefix: 'channel.', name: 'Notifications' },
  { prefix: 'watchdog.', name: 'Hub watchdog' },
  { prefix: 'statuspage.', name: 'Status pages' },
  { prefix: 'maintenance.', name: 'Maintenance' },
  { prefix: 'template_source.', name: 'Template indexes' },
  { prefix: 'auth.', name: 'Sign-in' },
];

const when = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
export const auditTime = (iso: string) => when.format(new Date(iso));
