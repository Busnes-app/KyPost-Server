import { PanelTabs } from "../../components/PanelTabs";
import { EmailServer } from "../../settings/sections/EmailServer";
import { SendAs } from "../../settings/sections/SendAs";
import { CardDavClient } from "../../settings/sections/CardDavClient";
import { Filters } from "../../settings/sections/Filters";
import { MailStorage } from "../../settings/sections/MailStorage";
import { KyCalendar } from "../../settings/sections/KyCalendar";
import { ReadingPane } from "../../settings/sections/ReadingPane";

/**
 * Everything about getting mail in and out: the server it comes from, the
 * addresses it can go out as, the contacts that ride alongside it, and the
 * rules applied on arrival.
 *
 * Import and export live under Security, behind its re-authentication.
 *
 * Tabbed rather than stacked — six sections, each with its own save action,
 * is more than one scroll can present clearly.
 */
export function MailPanel() {
  return (
    <section className="panel">
      <div className="config-header">
        <h2 className="visually-hidden">Mail</h2>
        <p>Your mail server, where messages open, mailbox storage, the addresses you send as, KyCalendar invitations, contact sync and mailbox rules.</p>
      </div>
      <PanelTabs
        ariaLabel="Mail sections"
        tabs={[
          { id: "email-server", label: "Email settings", body: <EmailServer /> },
          { id: "reading-pane", label: "Reading pane", body: <ReadingPane /> },
          { id: "storage", label: "Storage", body: <MailStorage /> },
          { id: "send-as", label: "Send-as addresses", body: <SendAs /> },
          { id: "kycalendar", label: "KyCalendar", body: <KyCalendar /> },
          { id: "carddav-client", label: "CardDAV client", body: <CardDavClient /> },
          { id: "rules", label: "Mailbox rules", body: <Filters /> }
        ]}
      />
    </section>
  );
}
