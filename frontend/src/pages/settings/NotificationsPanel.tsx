import { NotificationPrefs } from "../../settings/sections/NotificationPrefs";

export function NotificationsPanel() {
  return (
    <section className="panel">
      <div className="config-header">
        <h1>Notifications</h1>
        <p>How and when this device tells you about new mail.</p>
      </div>
      <div id="notification-prefs"><NotificationPrefs /></div>
    </section>
  );
}
