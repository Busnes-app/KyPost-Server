import { PanelTabs } from "../../components/PanelTabs";
import { EmailServer } from "../../settings/sections/EmailServer";
import { SendAs } from "../../settings/sections/SendAs";
import { CardDavClient } from "../../settings/sections/CardDavClient";
import { Filters } from "../../settings/sections/Filters";
import { MailExport } from "../../settings/sections/MailExport";

/**
 * Everything about getting mail in and out: the server it comes from, the
 * addresses it can go out as, the contacts that ride alongside it, and the
 * rules applied on arrival.
 *
 * Tabbed rather than stacked — five sections, each with its own save action,
 * is more than one scroll can present clearly.
 */
export function MailPanel() {
  return (
    <section className="panel">
      <div className="config-header">
        <h2>Mail</h2>
        <p>Your mail server, the addresses you send as, contact sync, mailbox rules, and mail export.</p>
      </div>
      <PanelTabs
        ariaLabel="Mail sections"
        tabs={[
          { id: "email-server", label: "Email Settings", body: <EmailServer /> },
          { id: "send-as", label: "Send-As Addresses", body: <SendAs /> },
          { id: "carddav-client", label: "CardDAV Client", body: <CardDavClient /> },
          { id: "rules", label: "Mailbox Rules", body: <Filters /> },
          { id: "export", label: "Export Mail", body: <MailExport /> }
        ]}
      />
    </section>
  );
}
