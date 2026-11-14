import { useEffect, useState } from "react";
import { getBytes, postJSON } from "../../api/client";
import type { AttachmentInfo } from "./types";
import { answerByMail, answerThroughKyCalendar, parseInvite, partType, priorAnswer, recordAnswer, rsvpAttendee, rsvpMail, type Invite, type InviteEvent, type Partstat, type PriorAnswer } from "./invite";

type Props = {
  attachment: AttachmentInfo;
  /** `messageId=…[&mailbox=…]`, as the reader's attachment links use. */
  query: string;
  messageId: string;
  /** IMAP folder holding the message; "" is INBOX. */
  folder: string;
  /** The message's From address. */
  sender: string;
  phishing: boolean;
  /** The user's addresses for this mailbox, primary first. */
  ownAddresses: string[];
};

const answers: [Partstat, string][] = [["ACCEPTED", "Accept"], ["TENTATIVE", "Maybe"], ["DECLINED", "Decline"]];
const answered: Record<Partstat, string> = { ACCEPTED: "accepted", TENTATIVE: "answered maybe", DECLINED: "declined" };

function changedText(changed: string[]): string {
  if (changed.length === 0) return "This invitation was updated since you answered.";
  const list = changed.length === 1 ? changed[0] : `${changed.slice(0, -1).join(", ")} and ${changed[changed.length - 1]}`;
  return `Changed since you answered: ${list}.`;
}

function when(e: InviteEvent): string {
  if (e.allDay) {
    const last = new Date(e.end.getFullYear(), e.end.getMonth(), e.end.getDate() - 1);
    const f = new Intl.DateTimeFormat(undefined, { dateStyle: "full" });
    return last > e.begin ? f.formatRange(e.begin, last) : f.format(e.begin);
  }
  const f = new Intl.DateTimeFormat(undefined, { dateStyle: "full", timeStyle: "short" });
  return e.end > e.begin ? f.formatRange(e.begin, e.end) : f.format(e.begin);
}

// InviteCard shows a calendar invitation from the message's own .ics part and
// answers it through KyCalendar, falling back to an iTIP REPLY by mail when
// KyCalendar is off. Every value is sender-controlled and rendered as text.
// Render it keyed by message so no state carries over to another message.
export function InviteCard({ attachment, query, messageId, folder, sender, phishing, ownAddresses }: Props) {
  const [invite, setInvite] = useState<Invite | null>(null);
  const [reload, setReload] = useState(0);
  const [phase, setPhase] = useState<"idle" | "busy" | "confirm" | "conflict" | "done" | "stuck">("idle");
  const [choice, setChoice] = useState<Partstat>("ACCEPTED");
  const [status, setStatus] = useState("");
  const [noEvent, setNoEvent] = useState(false);
  const [prior, setPrior] = useState<PriorAnswer>({ kind: "none" });

  useEffect(() => {
    let live = true;
    getBytes(`/api/mail/attachment?${query}&index=${attachment.index}`)
      .then((bytes) => live && setInvite(parseInvite(bytes)))
      .catch(() => live && setInvite(null));
    return () => {
      live = false;
    };
  }, [query, attachment.index, reload]);

  const attendeeKey = invite ? rsvpAttendee(invite.event, ownAddresses) : "";
  useEffect(() => {
    let live = true;
    setPrior({ kind: "none" });
    if (invite?.method === "REQUEST") {
      priorAnswer(invite.event, attendeeKey).then((p) => live && setPrior(p)).catch(() => undefined);
    }
    return () => {
      live = false;
    };
  }, [invite, attendeeKey]);

  if (!invite) return null;
  const e = invite.event;
  const cancelled = invite.method === "CANCEL";
  const offersRsvp = invite.method === "REQUEST" && e.canReply
    && partType(attachment) === "text/calendar" && attachment.calendarMethod === "REQUEST";
  const attendee = attendeeKey;
  let joinHost = "";
  try {
    joinHost = e.conference && !cancelled ? new URL(e.conference).hostname : "";
  } catch {
    joinHost = "";
  }
  const remember = (partstat: Partstat) => void recordAnswer(e, attendee, partstat).catch(() => undefined);

  async function answer(partstat: Partstat) {
    setChoice(partstat);
    setPhase("busy");
    setStatus("");
    const result = await answerThroughKyCalendar(postJSON, { messageId, mailbox: folder, index: attachment.index, partstat });
    if (result.kind === "answered") {
      remember(partstat);
      setPhase("done");
      setStatus("Answered through KyCalendar.");
    } else if (result.kind === "off") {
      setNoEvent(result.noEvent === true);
      setPhase("confirm");
    } else {
      setPhase(result.kind);
      setStatus(result.message);
    }
  }

  async function sendByMail() {
    const mail = rsvpMail(e, attendee, choice, new Date());
    if (!mail) {
      setPhase("idle");
      setStatus("No address of yours can answer this invitation.");
      return;
    }
    setPhase("busy");
    const result = await answerByMail(postJSON, mail, attendee);
    if (result.kind === "sent" || result.kind === "plain") remember(choice);
    setPhase(result.kind === "retry" ? "idle" : result.kind === "uncertain" ? "stuck" : "done");
    setStatus(result.kind === "sent" ? `Your answer was sent to ${e.organizer}.` : result.message);
  }

  return (
    <section className="invite-card" aria-label="Calendar invitation">
      <strong className="invite-card-title">{e.summary || "Invitation"}</strong>
      {cancelled ? <p className="invite-card-cancelled">Cancelled</p> : null}
      <dl className="invite-card-facts">
        <dt>When</dt>
        <dd>{when(e)}{e.repeats ? " (repeats)" : ""}</dd>
        {e.location ? (<><dt>Where</dt><dd>{e.location}</dd></>) : null}
        {e.organizer ? (<><dt>Organizer</dt><dd>{e.organizer}</dd></>) : null}
      </dl>
      {phishing ? <p className="invite-card-warning">This message was flagged as possible phishing.</p> : null}
      {e.organizer && sender && e.organizer.toLowerCase() !== sender.toLowerCase() ? (
        <p className="invite-card-warning">The organizer is not the sender ({sender}).</p>
      ) : null}
      {joinHost ? (
        <a className="btn-primary invite-card-join" href={e.conference} target="_blank" rel="noopener noreferrer">
          Join meeting <span className="invite-card-host">({joinHost})</span>
        </a>
      ) : null}
      {prior.kind === "newer" ? (
        <p className="invite-card-warning">{changedText(prior.changed)} You had {answered[prior.partstat]}; answer again.</p>
      ) : prior.kind === "same" ? (
        <p className="invite-card-note">You {answered[prior.partstat]}.</p>
      ) : invite.method === "REQUEST" && e.sequence > 0 ? <p className="invite-card-warning">This updates an earlier invitation.</p> : null}
      {e.unknownZone ? <p className="invite-card-warning">Time zone “{e.unknownZone}” is unknown; shown in your local time.</p> : null}
      {offersRsvp && phase === "confirm" ? (
        <div className="invite-card-actions">
          <p className="invite-card-note">
            {noEvent ? "KyCalendar has no event for this invitation" : "KyCalendar is not in use for this mailbox"}, so your answer goes to {e.organizer} as an unencrypted, unsigned email.
          </p>
          <button type="button" onClick={() => void sendByMail()}>Send answer</button>
          <button type="button" onClick={() => setPhase("idle")}>Cancel</button>
        </div>
      ) : offersRsvp && phase !== "done" ? (
        <div className="invite-card-actions" role="group" aria-label="Answer">
          {answers.map(([partstat, label]) => (
            <button key={partstat} type="button" disabled={phase !== "idle"} onClick={() => void answer(partstat)}>{label}</button>
          ))}
        </div>
      ) : null}
      {status ? <p className="invite-card-status" role="status">{status}</p> : null}
      {phase === "conflict" ? (
        <button type="button" onClick={() => { setPhase("idle"); setStatus(""); setInvite(null); setReload((n) => n + 1); }}>Reload invitation</button>
      ) : null}
    </section>
  );
}
