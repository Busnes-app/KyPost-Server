import { useRef } from "react";
import { useDialogOpen } from "../hooks/useDialogOpen";

type RulesHelpModalProps = {
  isOpen: boolean;
  onClose: () => void;
};

export function RulesHelpModal({ isOpen, onClose }: RulesHelpModalProps) {
  const dialogRef = useRef<HTMLDialogElement | null>(null);

  useDialogOpen(dialogRef, isOpen);

  return (
    <dialog
      ref={dialogRef}
      className="rules-help-backdrop"
      onCancel={(event) => {
        event.preventDefault();
        onClose();
      }}
      onClick={(event) => {
        if (event.target === dialogRef.current) {
          onClose();
        }
      }}
    >
      <div className="rules-help-window" onClick={(event) => event.stopPropagation()}>
        <div className="rules-help-head">
          <h3>How to write rules</h3>
          <button type="button" className="contacts-action" onClick={onClose}>
            Close
          </button>
        </div>

        <div className="rules-help-body">
          <p>
            Rules run top-to-bottom on every new message that arrives in INBOX. A <code>stop</code> action ends
            evaluation early, so earlier rules can prevent later ones from running. You can also run every
            enabled rule on demand against any folder using "Run rules now".
          </p>

          <h4>Conditions</h4>
          <p>Each condition checks one field of the message against a value.</p>
          <table className="rules-help-table">
            <thead>
              <tr>
                <th>Field</th>
                <th>Checks</th>
              </tr>
            </thead>
            <tbody>
              <tr><td><code>from</code></td><td>Sender header</td></tr>
              <tr><td><code>to</code></td><td>To header</td></tr>
              <tr><td><code>cc</code></td><td>Cc header</td></tr>
              <tr><td><code>bcc</code></td><td>Bcc header</td></tr>
              <tr><td><code>subject</code></td><td>Subject header</td></tr>
              <tr><td><code>body</code></td><td>Message body (only fetched when a rule actually needs it)</td></tr>
              <tr><td><code>keyword</code></td><td>Matches if any of the message's existing keywords/labels match</td></tr>
              <tr><td><code>header</code></td><td>Any header you name, e.g. <code>X-Spam-Flag</code> added by your mail provider's spam filter. Matches if any copy of that header matches</td></tr>
            </tbody>
          </table>

          <table className="rules-help-table">
            <thead>
              <tr>
                <th>Comparator</th>
                <th>Behavior</th>
              </tr>
            </thead>
            <tbody>
              <tr><td><code>contains</code></td><td>Case-insensitive substring match</td></tr>
              <tr><td><code>is</code></td><td>Case-insensitive exact match</td></tr>
              <tr><td><code>matches</code></td><td>Wildcard glob against the whole value — <code>*</code> matches any run of characters, <code>?</code> matches exactly one</td></tr>
              <tr><td><code>regex</code></td><td>Regular expression, case-insensitive, matched anywhere in the field</td></tr>
            </tbody>
          </table>

          <p>
            Check "NOT" on a condition to invert it. Choose <strong>ALL of the following (AND)</strong> to require
            every condition to match, or <strong>ANY of the following (OR)</strong> to match if just one does. The
            visual builder only supports one flat group of conditions — a rule with nested groups (only reachable
            by hand-editing its script) shows as script-only.
          </p>

          <h4>Actions</h4>
          <p>Actions run in order, top to bottom, for any rule whose conditions match.</p>
          <table className="rules-help-table">
            <thead>
              <tr>
                <th>Action</th>
                <th>Effect</th>
              </tr>
            </thead>
            <tbody>
              <tr><td><code>keyword</code></td><td>Add a keyword/label to the message</td></tr>
              <tr><td><code>unkeyword</code></td><td>Remove a keyword/label from the message</td></tr>
              <tr><td><code>move</code></td><td>File the message into the folder you specify</td></tr>
              <tr><td><code>read</code></td><td>Mark the message as read</td></tr>
              <tr><td><code>archive</code></td><td>Archive the message</td></tr>
              <tr><td><code>spam</code></td><td>Mark the message as spam</td></tr>
              <tr><td><code>delete</code></td><td>Delete the message</td></tr>
              <tr><td><code>stop</code></td><td>Stop evaluating any further rules for this message</td></tr>
            </tbody>
          </table>
          <p className="rules-help-note">
            <code>read</code>, <code>archive</code>, and <code>spam</code> are server custom commands specific to
            this app — they aren't part of the Sieve mail-filtering standard. Everything else (<code>keyword</code>/
            <code>unkeyword</code> as <code>addflag</code>/<code>removeflag</code>, <code>move</code> as{" "}
            <code>fileinto</code>, <code>delete</code> as <code>discard</code>, and <code>stop</code>) maps directly
            to standard Sieve.
          </p>

          <h4>Writing rules as a script</h4>
          <p>
            "View as script" shows and edits a rule using this server's Sieve-like syntax directly. Custom actions
            are gated behind a <code>"kymail"</code> capability so it's clear at a glance which parts are
            non-standard:
          </p>
          <pre className="rules-help-code">{`require ["fileinto", "imap4flags", "kymail"];

if header :contains "from" "billing@example.com" {
    addflag "Bills";
    fileinto "Finance";
    stop;
}`}</pre>
          <p>
            Supported tests: <code>header</code> (any header name, e.g.{" "}
            <code>header :is ["X-Spam-Flag"] "YES"</code>), <code>address</code> (from/to/cc/bcc),{" "}
            <code>exists</code>,{" "}
            <code>body</code>, <code>hasflag</code>, combined with <code>allof(...)</code>, <code>anyof(...)</code>,
            and <code>not</code>. Comparators are given as a tag, e.g. <code>:contains</code>, <code>:is</code>,{" "}
            <code>:matches</code>, <code>:regex</code>. Comments use <code>#</code> for a line or{" "}
            <code>/* ... */</code> for a block. Condition groups can nest up to 32 levels deep. Values are always
            literal strings — there's no variable or template support.
          </p>

          <h4>Scope</h4>
          <p>
            The automatic poller only ever processes new INBOX mail, regardless of scope. Folder scoping only
            affects "Run rules now" against a chosen folder, and isn't currently editable from this page.
          </p>

          <h4>Other resources</h4>
          <ul className="rules-help-links">
            <li>
              <a href="https://www.rfc-editor.org/rfc/rfc5228" target="_blank" rel="noreferrer">
                RFC 5228 — Sieve: An Email Filtering Language
              </a>{" "}
              (base language this server's rule engine is modeled on)
            </li>
            <li>
              <a href="https://www.rfc-editor.org/rfc/rfc5232" target="_blank" rel="noreferrer">
                RFC 5232 — Sieve Email Filtering: Imap4flags Extension
              </a>{" "}
              (<code>addflag</code>/<code>removeflag</code>/<code>hasflag</code>, i.e. <code>keyword</code>/
              <code>unkeyword</code>)
            </li>
            <li>
              <a href="https://www.rfc-editor.org/rfc/rfc5173" target="_blank" rel="noreferrer">
                RFC 5173 — Sieve Email Filtering: Body Extension
              </a>{" "}
              (the <code>body</code> condition)
            </li>
          </ul>
          <p className="rules-help-note">
            The <code>regex</code> comparator is a custom extension beyond core Sieve — there's no standard RFC
            for it (Sieve regex matching was only ever an expired Internet-Draft), so treat it as another server
            custom command like <code>read</code>/<code>archive</code>/<code>spam</code>.
          </p>
        </div>
      </div>
    </dialog>
  );
}
