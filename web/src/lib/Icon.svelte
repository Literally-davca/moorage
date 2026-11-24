<script lang="ts" module>
  // Tabler outline icons, bundled as raw SVG (no icon font, no CDN).
  import anchor from '@tabler/icons/outline/anchor.svg?raw';
  import search from '@tabler/icons/outline/search.svg?raw';
  import plus from '@tabler/icons/outline/plus.svg?raw';
  import play from '@tabler/icons/outline/player-play.svg?raw';
  import stop from '@tabler/icons/outline/player-stop.svg?raw';
  import refresh from '@tabler/icons/outline/refresh.svg?raw';
  import download from '@tabler/icons/outline/download.svg?raw';
  import trash from '@tabler/icons/outline/trash.svg?raw';
  import lock from '@tabler/icons/outline/lock.svg?raw';
  import dots from '@tabler/icons/outline/dots.svg?raw';
  import x from '@tabler/icons/outline/x.svg?raw';
  import check from '@tabler/icons/outline/check.svg?raw';
  import alert from '@tabler/icons/outline/alert-triangle.svg?raw';
  import logout from '@tabler/icons/outline/logout.svg?raw';
  import sun from '@tabler/icons/outline/sun.svg?raw';
  import moon from '@tabler/icons/outline/moon.svg?raw';
  import arrowDown from '@tabler/icons/outline/arrow-down.svg?raw';
  import arrowUp from '@tabler/icons/outline/arrow-up.svg?raw';
  import pause from '@tabler/icons/outline/player-pause.svg?raw';
  import eraser from '@tabler/icons/outline/eraser.svg?raw';
  import terminal from '@tabler/icons/outline/terminal-2.svg?raw';
  import server from '@tabler/icons/outline/server.svg?raw';
  import power from '@tabler/icons/outline/power.svg?raw';
  import cloudDown from '@tabler/icons/outline/cloud-download.svg?raw';
  import wand from '@tabler/icons/outline/wand.svg?raw';
  import menu from '@tabler/icons/outline/menu-2.svg?raw';
  import chevronLeft from '@tabler/icons/outline/chevron-left.svg?raw';
  import wrap from '@tabler/icons/outline/text-wrap.svg?raw';
  import history from '@tabler/icons/outline/history.svg?raw';
  import rollback from '@tabler/icons/outline/arrow-back-up.svg?raw';
  import selector from '@tabler/icons/outline/selector.svg?raw';
  import copy from '@tabler/icons/outline/copy.svg?raw';
  import plug from '@tabler/icons/outline/plug-connected.svg?raw';
  import pulse from '@tabler/icons/outline/activity-heartbeat.svg?raw';
  import bell from '@tabler/icons/outline/bell.svg?raw';
  import world from '@tabler/icons/outline/world.svg?raw';
  import clipboardList from '@tabler/icons/outline/clipboard-list.svg?raw';
  import tool from '@tabler/icons/outline/tool.svg?raw';
  import hierarchy from '@tabler/icons/outline/hierarchy.svg?raw';
  import template from '@tabler/icons/outline/template.svg?raw';
  import eye from '@tabler/icons/outline/eye.svg?raw';
  import eyeOff from '@tabler/icons/outline/eye-off.svg?raw';
  import external from '@tabler/icons/outline/external-link.svg?raw';
  import users from '@tabler/icons/outline/users.svg?raw';
  import userCircle from '@tabler/icons/outline/user-circle.svg?raw';
  import key from '@tabler/icons/outline/key.svg?raw';
  import chevronDown from '@tabler/icons/outline/chevron-down.svg?raw';
  import settings from '@tabler/icons/outline/settings.svg?raw';
  import info from '@tabler/icons/outline/info-circle.svg?raw';
  import zoomIn from '@tabler/icons/outline/zoom-in.svg?raw';
  import zoomOut from '@tabler/icons/outline/zoom-out.svg?raw';
  import fit from '@tabler/icons/outline/arrows-maximize.svg?raw';
  import columns from '@tabler/icons/outline/layout-columns.svg?raw';
  import logs from '@tabler/icons/outline/file-text.svg?raw';
  import shield from '@tabler/icons/outline/shield-check.svg?raw';
  import clock from '@tabler/icons/outline/clock.svg?raw';
  import filter from '@tabler/icons/outline/filter.svg?raw';
  import link from '@tabler/icons/outline/link.svg?raw';
  import puzzle from '@tabler/icons/outline/puzzle.svg?raw';
  import robot from '@tabler/icons/outline/robot.svg?raw';
  import webhook from '@tabler/icons/outline/webhook.svg?raw';
  import upload from '@tabler/icons/outline/upload.svg?raw';
  import bolt from '@tabler/icons/outline/bolt.svg?raw';
  import calendar from '@tabler/icons/outline/calendar-time.svg?raw';
  import merge from '@tabler/icons/outline/git-merge.svg?raw';
  import swap from '@tabler/icons/outline/arrows-exchange.svg?raw';
  import code from '@tabler/icons/outline/code.svg?raw';

  const icons = {
    anchor, search, plus, play, stop, refresh, download, trash, lock, dots, x, check, alert, logout, sun, moon,
    arrowDown, arrowUp, pause, eraser, terminal, server, power, cloudDown, wand, menu, chevronLeft, wrap, history, rollback, selector, copy, plug, pulse, bell, world, clipboardList, tool, hierarchy,
    template, eye, eyeOff, external, users, userCircle, key, chevronDown, settings,
    info, zoomIn, zoomOut, fit, columns, logs, shield, clock, filter, link, puzzle, robot, webhook, upload, bolt, calendar, merge, swap, code,

  };
  export type IconName = keyof typeof icons;
</script>

<script lang="ts">
  let { name, label }: { name: IconName; label?: string } = $props();
</script>

<span class="icon" role={label ? 'img' : undefined} aria-label={label} aria-hidden={label ? undefined : 'true'}>
  {@html icons[name]}
</span>
