package session

import "log/slog"

// notifyAsks raises one desktop notification per new "asks you" in this
// profile ([notifications] desktop = true). The claim is durable
// (MarkAskNotified), so a daemon restart, a second pass or a set seen
// twice never notifies twice. The phone gets its own one through web push.
func (d *TransitionDaemon) notifyAsks(profile string, byID map[string]*Instance) {
	if d == nil || d.deskNotifier == nil || !GetNotificationsSettings().GetDesktopEnabled() {
		return
	}
	for id, ask := range OpenAsks() {
		inst := byID[id]
		if inst == nil || inst.IsArchived() || !MarkAskNotified(id, ask, "desktop") {
			continue
		}
		title, body := AskNotificationText(inst.Title, ask)
		// Off the poll loop, like notifyDesktop: the notifier shells out.
		d.desktopWG.Add(1)
		go func() {
			defer d.desktopWG.Done()
			if backend := d.deskNotifier.NotifyText(title, body); backend != "" {
				sessionLog.Debug("asks_desktop_notification_sent",
					slog.String("instance_id", id), slog.String("profile", profile), slog.String("backend", backend))
			}
		}()
	}
}
