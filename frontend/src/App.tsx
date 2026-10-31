import { type DragEvent, type MouseEvent as ReactMouseEvent, type ReactElement, lazy, Suspense, useEffect, useEffectEvent, useRef, useState } from "react";
import { Link, Navigate, Route, Routes, useLocation, useNavigate } from "react-router";
import { deleteJSON, getJSON, onMailboxGone, postJSON, putJSON, selectedMailboxId, selectMailbox, toErrorMessage } from "./api/client";
import { AuthContext, type AuthState } from "./auth";
import { ComposeWindow } from "./components/ComposeWindow";
import { useCompose } from "./hooks/useCompose";
import { useDialogOpen } from "./hooks/useDialogOpen";
import { clearPGPSession, loadPGPSession } from "./lib/pgpSession";
import { LoginPage } from "./pages/LoginPage";
import { ReauthGate, clearReauth } from "./components/ReauthGate";
import mitLicenseText from "./mit.txt?raw";

import { visibleSettingsGroups } from "./app/navigation";
import { loadMailboxSelection, MailboxContext, mailboxName, readMailboxes, saveMailboxSelection, type UserMailbox } from "./app/mailboxes";
import { LEGACY_SETTINGS_PATHS, legacySettingsRedirect } from "./app/routes";
import { useFolderList } from "./hooks/useFolderList";
import { subscribeSecretHold } from "./lib/secretHold";
import type {
  BeforeInstallPromptEvent,
  InboxFolder,
  CreateFolderResponse,
  DeleteFolderResponse,
  RenameFolderResponse,
  MoveInboxActionResponse,
  DragMessagePayload
} from "./app/types";
import { clearDraftSnapshot } from "./app/draftAutosave";
import { mailboxLabel, standardMailboxKind, standardMailboxPath, type StandardMailboxKind } from "./app/mailboxKind";
import {
  AppMark, ArchiveIcon, ChevronIcon, ClockIcon, ContactsIcon, DraftIcon, FolderIcon, InboxIcon, LogoutIcon, PaletteIcon, PlusIcon, SendIcon, SettingsIcon, TrashIcon, WarningIcon
} from "./pages/read/icons";

const FOLDER_ICONS: Record<StandardMailboxKind | "custom", () => ReactElement> = {
  inbox: InboxIcon, drafts: DraftIcon, junk: WarningIcon, sent: SendIcon, trash: TrashIcon, custom: FolderIcon
};

// Every page below is a separate chunk. Only LoginPage is eager: it is what an
// unauthenticated visitor lands on, so putting it behind a second round trip
// would slow down the one route that has to be fast.
const ScheduledPage = lazy(() => import("./pages/ScheduledPage").then((m) => ({ default: m.ScheduledPage })));
const AppsPage = lazy(() => import("./pages/AppsPage").then((m) => ({ default: m.AppsPage })));
const ContactsPage = lazy(() => import("./pages/ContactsPage").then((m) => ({ default: m.ContactsPage })));
const ReadPage = lazy(() => import("./pages/ReadPage").then((m) => ({ default: m.ReadPage })));
const SecurityPage = lazy(() => import("./pages/SecurityPage").then((m) => ({ default: m.SecurityPage })));
const AppearancePanel = lazy(() => import("./pages/settings/AppearancePanel").then((m) => ({ default: m.AppearancePanel })));
const MailPanel = lazy(() => import("./pages/settings/MailPanel").then((m) => ({ default: m.MailPanel })));
const NotificationsPanel = lazy(() => import("./pages/settings/NotificationsPanel").then((m) => ({ default: m.NotificationsPanel })));
const StatusPanel = lazy(() => import("./pages/settings/StatusPanel").then((m) => ({ default: m.StatusPanel })));
const AutomationPanel = lazy(() => import("./pages/settings/AutomationPanel").then((m) => ({ default: m.AutomationPanel })));
const ServerPanel = lazy(() => import("./pages/admin/ServerPanel").then((m) => ({ default: m.ServerPanel })));
const DiagnosticsPanel = lazy(() => import("./pages/admin/DiagnosticsPanel").then((m) => ({ default: m.DiagnosticsPanel })));

export function App() {
  const location = useLocation();
  const navigate = useNavigate();
  const [auth, setAuth] = useState<AuthState | null>(null);
  // The caller's mailboxes and the selected one ("" is the primary); see app/mailboxes.ts.
  const [mailboxes, setMailboxes] = useState<UserMailbox[]>([]);
  const [selectedMailbox, setSelectedMailbox] = useState("");
  const [mailboxesVersion, setMailboxesVersion] = useState(0);
  const [mailboxNotice, setMailboxNotice] = useState("");
  const compose = useCompose({ userId: auth?.userId, mailboxes });
  // Folder URLs name their mailbox, so one replayed after a switch is recognised (ReadPage).
  const boxQuery = selectedMailbox ? `&box=${selectedMailbox}` : "";
  const mailboxFolderList = useFolderList("/api/inbox/folders");
  const { folders: mailboxFolders, loading: mailboxFoldersLoading } = mailboxFolderList;
  const [inboxCreateOpen, setInboxCreateOpen] = useState(false);
  const [createFolderName, setCreateFolderName] = useState("");
  const [createFolderLoading, setCreateFolderLoading] = useState(false);
  const [createFolderError, setCreateFolderError] = useState("");
  const [archiveOpen, setArchiveOpen] = useState(false);
  const archiveFolderList = useFolderList("/api/inbox/folders?parent=Archive");
  const { folders: archiveFolders, loading: archiveFoldersLoading } = archiveFolderList;
  const [folderMenuPath, setFolderMenuPath] = useState("");
  const [deleteFolderLoading, setDeleteFolderLoading] = useState("");
  const [renameFolderLoading, setRenameFolderLoading] = useState("");
  const [deleteFolderError, setDeleteFolderError] = useState("");
  const [dragOverFolder, setDragOverFolder] = useState("");
  // On a phone the nav is one sideways-scrolling strip; keep the current page's link in view.
  const navRef = useRef<HTMLElement | null>(null);
  useEffect(() => {
    navRef.current
      ?.querySelectorAll(".sidebar-link-active")
      .forEach((link) => link.scrollIntoView({ block: "nearest", inline: "nearest" }));
  }, [location.pathname, location.search, auth?.authenticated, mailboxFolders]);
  const [pwaInstallPrompt, setPwaInstallPrompt] = useState<BeforeInstallPromptEvent | null>(null);
  const [pwaInstalled, setPwaInstalled] = useState(false);
  const [licenseOpen, setLicenseOpen] = useState(false);
  const licenseDialogRef = useRef<HTMLDialogElement | null>(null);
  const [appVersion, setAppVersion] = useState("");
  const currentMailbox = new URLSearchParams(location.search).get("mailbox")?.trim() ?? "";
  const onReadPage = location.pathname === "/read";
  const onAppearance = location.pathname === "/settings/appearance";
  // Every settings and admin panel, and the apps page, sits under the one Settings item.
  const onSettings = !onAppearance && ["/settings", "/admin", "/apps"].some((prefix) => location.pathname.startsWith(prefix));

  // Navigation hold. A section showing a value the server issues once and
  // cannot reissue (a generated CardDAV app password) registers a hold; any
  // sidebar link would unmount it and destroy the only copy. The tabbed pages
  // guard their own tab strips, but a link is a route change and never reaches
  // that guard — this is the other half of the same protection.
  const [secretHold, setSecretHold] = useState("");
  const [navBlockedNotice, setNavBlockedNotice] = useState("");
  useEffect(() => subscribeSecretHold(setSecretHold), []);
  useEffect(() => {
    if (!secretHold) {
      setNavBlockedNotice("");
    }
  }, [secretHold]);

  function guardNavigation(event: ReactMouseEvent<HTMLElement>) {
    if (!secretHold) {
      return;
    }
    // Links only. The buttons inside this nav (folder and Archive disclosures,
    // Sign out) either do not navigate or are a deliberate exit, and
    // trapping someone signing out would be worse than the loss it prevents.
    if (!(event.target as HTMLElement).closest("a")) {
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    setNavBlockedNotice(secretHold);
  }

  async function refreshAuth() {
    try {
      const next = await getJSON<AuthState>("/api/auth/me");
      // Before the first render of a mail page, so its first request already follows the selection.
      const restored = next.authenticated && next.userId ? loadMailboxSelection(next.userId) : "";
      selectMailbox(restored);
      setSelectedMailbox(restored);
      setAuth(next);
    } catch {
      setAuth({ authenticated: false });
    }
  }

  useEffect(() => {
    refreshAuth();
  }, []);

  function chooseMailbox(id: string) {
    selectMailbox(id);
    setSelectedMailbox(id);
    if (auth?.userId) saveMailboxSelection(auth.userId, id);
    // Folder lists belong to the mailbox they were read from; ReadPage remounts on the key below.
    mailboxFolderList.reset();
    archiveFolderList.reset();
  }

  // Called from async answers, so it must see the current sign-in and folder lists.
  const fallBackToPrimary = useEffectEvent((notice: string) => {
    chooseMailbox("");
    setMailboxNotice(notice);
  });

  // A non-native account gets an empty list, and no header is ever sent for it.
  useEffect(() => {
    const userId = auth?.authenticated ? auth.userId : undefined;
    if (!userId) {
      setMailboxes([]);
      return;
    }
    let live = true;
    getJSON<unknown>("/api/mailboxes")
      .then((data) => readMailboxes(data, userId))
      .then(
        (list) => {
          if (!live) return;
          setMailboxes(list);
          if (selectedMailboxId() && !list.some((m) => m.id === selectedMailboxId())) {
            fallBackToPrimary("This mailbox is no longer available. Showing your primary mailbox.");
          }
        },
        () => {
          if (!live) return;
          setMailboxes([]);
          if (selectedMailboxId()) {
            fallBackToPrimary("Your mailboxes could not be loaded. Showing your primary mailbox.");
          }
        }
      );
    return () => {
      live = false;
    };
  }, [auth?.authenticated, auth?.userId, mailboxesVersion]);

  // Any mail request refused with 404 "mailbox not found" lands here.
  useEffect(() => {
    onMailboxGone((id) => {
      if (id !== selectedMailboxId()) return;
      fallBackToPrimary("This mailbox is no longer available. Showing your primary mailbox.");
      setMailboxesVersion((v) => v + 1);
    });
  }, [auth?.userId]);


  // Cold start: the unwrapped PGP key cannot survive a reload, so every
  // authenticated page load re-fetches the snapshot that says whether this
  // account has a key, which mode it is under, and whether an unlock is
  // needed. Nothing is unlocked here — that waits until something needs it.
  useEffect(() => {
    if (auth?.authenticated) {
      void loadPGPSession();
    }
  }, [auth?.authenticated]);


  // ponytail: single backend source for the displayed version (serverVersion)
  // — both the About overlay and Admin > Server read the same const via API.
  useEffect(() => {
    if (!auth?.authenticated) {
      setAppVersion("");
      return;
    }
    let cancelled = false;
    getJSON<{ version: string }>("/api/status")
      .then((data) => {
        if (!cancelled && data.version) setAppVersion(data.version);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [auth?.authenticated]);

  useEffect(() => {
    const standalone = window.matchMedia("(display-mode: standalone)").matches ||
      (window.navigator as Navigator & { standalone?: boolean }).standalone === true;
    setPwaInstalled(standalone);

    function onBeforeInstallPrompt(event: Event) {
      event.preventDefault();
      setPwaInstallPrompt(event as BeforeInstallPromptEvent);
    }

    function onAppInstalled() {
      setPwaInstallPrompt(null);
      setPwaInstalled(true);
    }

    window.addEventListener("beforeinstallprompt", onBeforeInstallPrompt);
    window.addEventListener("appinstalled", onAppInstalled);
    return () => {
      window.removeEventListener("beforeinstallprompt", onBeforeInstallPrompt);
      window.removeEventListener("appinstalled", onAppInstalled);
    };
  }, []);

  // unsubscribeThisDevice removes the browser's push registration, server-side
  // then locally. Best-effort: a failure here must never block signing out.
  async function unsubscribeThisDevice() {
    try {
      if (!("serviceWorker" in navigator)) return;
      const reg = await navigator.serviceWorker.getRegistration();
      const subscription = await reg?.pushManager.getSubscription();
      if (!subscription) return;
      await deleteJSON<{ ok: boolean }>("/api/notifications/subscriptions", {
        endpoint: subscription.endpoint
      });
      await subscription.unsubscribe();
    } catch {
      // Ignored on purpose: logout must proceed regardless.
    }
  }

  async function logout() {
    try {
      // Drop this browser's push subscription BEFORE the session goes away.
      // The DELETE route requires a live session, so doing it afterwards would
      // 401 and leave the row in place — and a web-push subscription is not
      // tied to the session, so the previous user's mail notifications would
      // keep arriving on a shared browser indefinitely.
      await unsubscribeThisDevice();
      await postJSON<{ ok: boolean }>("/api/auth/logout", {});
    } finally {
      chooseMailbox("");
      setMailboxNotice("");
      // Drop the unwrapped private key with the session. Leaving it in memory
      // after logout would let the next person at this browser read mail.
      clearPGPSession();
      // Same reasoning for the security page's re-auth: it is keyed to the
      // username, so a second sign-in as someone else could not inherit it, but
      // signing back in as the SAME user within the window would have walked
      // straight past a gate whose whole subject is who is sitting here.
      clearReauth();
      // Drop the autosaved compose buffer with the session, for the same
      // reason: the next person at this browser must not read it.
      if (auth?.userId) {
        clearDraftSnapshot(auth.userId);
      }
      setAuth({ authenticated: false });
    }
  }

  async function installPwa() {
    const prompt = pwaInstallPrompt;
    if (!prompt) {
      return;
    }
    // Consume first: the event's prompt() may be called once, and a second
    // click while the dialog is up would throw.
    setPwaInstallPrompt(null);
    await prompt.prompt();
    const choice = await prompt.userChoice;
    if (choice.outcome === "accepted") {
      setPwaInstalled(true);
    }
  }

  async function loadMailboxFolders() {
    if (!auth?.authenticated) {
      mailboxFolderList.reset();
      return;
    }
    await mailboxFolderList.load();
  }

  async function createInboxFolder() {
    const name = createFolderName.trim();
    if (!name) {
      setCreateFolderError("Folder name is required.");
      return;
    }
    setCreateFolderLoading(true);
    setCreateFolderError("");
    setDeleteFolderError("");
    try {
      await postJSON<CreateFolderResponse>("/api/inbox/folders", {
        parent: "INBOX",
        name
      });
      setCreateFolderName("");
      setInboxCreateOpen(false);
      await loadMailboxFolders();
    } catch (e) {
      const message = toErrorMessage(e, "failed to create folder");
      setCreateFolderError(message);
    } finally {
      setCreateFolderLoading(false);
    }
  }

  async function loadArchiveFolders() {
    if (!auth?.authenticated) {
      archiveFolderList.reset();
      return;
    }
    await archiveFolderList.load();
  }

  async function deleteInboxFolder(folder: InboxFolder) {
    if (!folder.deletable || deleteFolderLoading || renameFolderLoading) return;
    const confirmed = window.confirm(`Delete ${mailboxLabel(folder.path)} and move its emails to ${mailboxLabel(folder.path.slice(0, Math.max(folder.path.lastIndexOf("/"), folder.path.lastIndexOf(".")))) || "the parent folder"}?`);
    if (!confirmed) return;

    setDeleteFolderLoading(folder.path);
    setFolderMenuPath("");
    setDeleteFolderError("");
    setCreateFolderError("");
    try {
      await deleteJSON<DeleteFolderResponse>(`/api/inbox/folders?folder=${encodeURIComponent(folder.path)}`);
      const params = new URLSearchParams(location.search);
      if (location.pathname === "/read" && params.get("mailbox") === folder.path) {
        navigate("/read", { replace: true });
      }
      await loadMailboxFolders();
    } catch (e) {
      const message = toErrorMessage(e, "failed to delete folder");
      setDeleteFolderError(message);
    } finally {
      setDeleteFolderLoading("");
    }
  }

  async function renameInboxFolder(folder: InboxFolder) {
    if (!folder.deletable || renameFolderLoading || deleteFolderLoading) return;
    const current = mailboxLabel(folder.path);
    const nextName = window.prompt("Rename folder", current) ?? "";
    const name = nextName.trim();
    if (!name || name === current) {
      setFolderMenuPath("");
      return;
    }

    setRenameFolderLoading(folder.path);
    setFolderMenuPath("");
    setDeleteFolderError("");
    setCreateFolderError("");
    try {
      const response = await putJSON<RenameFolderResponse>("/api/inbox/folders", {
        folder: folder.path,
        name
      });
      const params = new URLSearchParams(location.search);
      if (location.pathname === "/read" && params.get("mailbox") === folder.path) {
        navigate(`/read?mailbox=${encodeURIComponent(response.renamed)}${boxQuery}`, { replace: true });
      }
      await loadMailboxFolders();
    } catch (e) {
      const message = toErrorMessage(e, "failed to rename folder");
      setDeleteFolderError(message);
    } finally {
      setRenameFolderLoading("");
    }
  }

  function parseDragPayload(raw: string): DragMessagePayload | null {
    try {
      const parsed = JSON.parse(raw) as DragMessagePayload;
      if (!Array.isArray(parsed.messageIds) || parsed.messageIds.length === 0) return null;
      const messageIds = parsed.messageIds.map((value) => String(value).trim()).filter(Boolean);
      const mailbox = String(parsed.mailbox || "").trim();
      if (messageIds.length === 0 || mailbox === "") return null;
      return { messageIds, mailbox };
    } catch {
      return null;
    }
  }

  async function moveDraggedMessages(targetMailbox: string, event: DragEvent<HTMLElement>) {
    event.preventDefault();
    setDragOverFolder("");
    const payload = parseDragPayload(event.dataTransfer.getData("application/x-kypost-mailbox"));
    if (!payload) return;
    if (payload.mailbox.toLowerCase() === targetMailbox.toLowerCase()) return;

    setDeleteFolderError("");
    setCreateFolderError("");
    try {
      const response = await postJSON<MoveInboxActionResponse>("/api/inbox/actions", {
        action: "move",
        mailbox: payload.mailbox,
        targetMailbox,
        messageIds: payload.messageIds
      });
      if (response.failed.length > 0) {
        throw new Error(response.failed[0]?.error || "some emails could not be moved");
      }
      window.dispatchEvent(new CustomEvent("mailbox-move-complete", {
        detail: {
          sourceMailbox: payload.mailbox,
          targetMailbox
        }
      }));
    } catch (e) {
      const message = toErrorMessage(e, "failed to move email");
      setDeleteFolderError(message);
    }
  }

  // Re-read on sign-in and on every mailbox switch; signing out clears the sidebar.
  const syncMailboxFolders = useEffectEvent(() => {
    if (!auth?.authenticated) {
      mailboxFolderList.reset();
      setInboxCreateOpen(false);
      setCreateFolderError("");
      setCreateFolderName("");
      setFolderMenuPath("");
      setDeleteFolderError("");
      setDragOverFolder("");
      return;
    }
    void loadMailboxFolders();
  });
  useEffect(() => {
    syncMailboxFolders();
  }, [auth?.authenticated, selectedMailbox]);

  const reloadArchiveFolders = useEffectEvent(loadArchiveFolders);
  useEffect(() => {
    if (!archiveOpen) return;
    void reloadArchiveFolders();
  }, [archiveOpen, auth?.authenticated, selectedMailbox]);


  useDialogOpen(licenseDialogRef, licenseOpen);




  if (auth === null) {
    // Same field as the sign-in page, so resolving the session does not flash a
    // differently-coloured screen on the way to it.
    return (
      <main className="ky-auth">
        <div className="ky-auth-theme" />
        <div className="ky-auth-body">
          <p className="ky-auth-help">Checking your session…</p>
        </div>
      </main>
    );
  }

  const isAdmin = auth.role === "admin";

  // Sign-in gets its own full-page layout rather than the app shell. Everything
  // in that shell — New Email, the mailbox list, the folder tree — is mail
  // chrome a signed-out visitor cannot use, and wrapping the one thing they CAN
  // do inside it made the front door read as a broken mail client. /password is
  // deliberately NOT here: that route is reached from inside the app and stays
  // an ordinary in-app panel.
  if (location.pathname === "/login") {
    return <LoginPage auth={auth} onAuthChanged={refreshAuth} />;
  }

  // ReactElement, not the global JSX.Element: @types/react 19 stopped
  // declaring a global JSX namespace, so the old spelling only resolved while
  // the types lagged a major behind the react 19 runtime we actually ship.
  function protect(element: ReactElement, adminOnly = false) {
    if (!auth?.authenticated) {
      return <Navigate to="/login" replace />;
    }
    if (adminOnly && !isAdmin) {
      return <Navigate to="/read" replace />;
    }
    return element;
  }

  // Settings is one page: its sections are one row of tabs at the top of the
  // content, above the open panel, so the sidebar stays the only side column.
  // On phones the row scrolls and also carries the licence line, which the
  // phone header hides.
  function settingsPage(panel: ReactElement) {
    return (
      <div className="settings-layout">
        <nav className="settings-sections" aria-label="Settings" onClickCapture={guardNavigation}>
          {visibleSettingsGroups(isAdmin).map((group) => (
            <div key={group.heading ?? "settings"} className="settings-section-group">
              {group.heading ? <span className="visually-hidden">{group.heading}</span> : null}
              {group.items.map(({ to, label }) => (
                <Link key={to} to={to} className="settings-section-link" aria-current={to === location.pathname ? "page" : undefined}>
                  {label}
                </Link>
              ))}
            </div>
          ))}
          {/* Not a settings panel: its own group after a divider. */}
          <div className="settings-section-group settings-section-detached">
            <Link to="/apps" className="settings-section-link" aria-current={location.pathname === "/apps" ? "page" : undefined}>
              Get the apps
            </Link>
          </div>
          <p className="settings-about">
            <button type="button" className="ky-license-link" onClick={() => setLicenseOpen(true)}>
              &copy; {new Date().getFullYear()} &middot; MIT License
            </button>
          </p>
        </nav>
        {panel}
      </div>
    );
  }

  return (
    <AuthContext.Provider value={auth}>
    <MailboxContext.Provider value={{ mailboxes, selected: selectedMailbox }}>
    <div className="shell">
      <aside className="sidebar">
        <div className="sidebar-logo">
          <AppMark size={28} />
          <span className="sidebar-wordmark">KyPost</span>
        </div>
        {/* Outlined on every page: each page has its own primary action. */}
        <button type="button" className="new-email-button" onClick={compose.openComposeWindow}>
          New Email
        </button>
        {navBlockedNotice ? (
          <p className="notice notice-error" role="alert">
            {navBlockedNotice}
          </p>
        ) : null}
        {mailboxes.length > 1 ? (
          <label className="mailbox-switcher">
            <span className="sidebar-section-label mailbox-switcher-label">Your mailboxes</span>
            <select
              value={selectedMailbox}
              onChange={(event) => {
                // Same hold as the links: switching opens /read and would
                // unmount a once-only secret.
                if (secretHold) {
                  setNavBlockedNotice(secretHold);
                  return;
                }
                chooseMailbox(event.target.value);
                setMailboxNotice("");
                navigate("/read");
              }}
            >
              {mailboxes.map((m) => (
                <option key={m.id} value={m.kind === "primary" ? "" : m.id}>
                  {mailboxName(m)}
                </option>
              ))}
            </select>
          </label>
        ) : null}
        {/* Browsers leave a partly visible item clipped when it takes focus, which the phone strip makes common. */}
        <nav
          ref={navRef}
          onClickCapture={guardNavigation}
          onFocus={(event) => event.target.scrollIntoView({ block: "nearest", inline: "nearest" })}
        >
          <p className="sidebar-section-label">Mailboxes</p>
          <div className="mobile-quick-nav" aria-label="Mobile mailboxes">
            <Link className={onReadPage && currentMailbox === "" ? "ky-nav-item sidebar-link-active" : "ky-nav-item"} to="/read">
              <InboxIcon />
              Inbox
            </Link>
            {/* Stand-ins for the folder rows the phone CSS hides by data-folder-kind,
                so each points at the account's real folder ("Sent Items" too). */}
            {([["drafts", "Drafts"], ["junk", "Junk"], ["sent", "Sent"], ["trash", "Trash"]] as const).map(([kind, label]) => {
              const path = standardMailboxPath(mailboxFolders, kind, label);
              const KindIcon = FOLDER_ICONS[kind];
              return (
                <Link
                  key={kind}
                  className={onReadPage && currentMailbox.toLowerCase() === path.toLowerCase() ? "ky-nav-item sidebar-link-active" : "ky-nav-item"}
                  to={`/read?mailbox=${encodeURIComponent(path)}${boxQuery}`}
                >
                  <KindIcon />
                  {label}
                </Link>
              );
            })}
            <Link className={location.pathname === "/scheduled" ? "ky-nav-item sidebar-link-active" : "ky-nav-item"} to="/scheduled">
              <ClockIcon />
              Scheduled
            </Link>
          </div>
          <div className="inbox-nav-row">
            <Link
              to="/read"
              className={[dragOverFolder === "INBOX" ? "drop-target-active" : "", onReadPage && currentMailbox === "" ? "ky-nav-item sidebar-link-active" : "ky-nav-item"].filter(Boolean).join(" ")}
              onDragOver={(event) => {
                event.preventDefault();
                setDragOverFolder("INBOX");
              }}
              onDragLeave={() => setDragOverFolder("")}
              onDrop={(event) => {
                void moveDraggedMessages("INBOX", event);
              }}
            >
              <InboxIcon />
              Inbox
            </Link>
            <button
              type="button"
              className="inbox-expand-button"
              aria-expanded={inboxCreateOpen}
              aria-label="New folder under Inbox"
              title="New folder under Inbox"
              onClick={() => {
                setInboxCreateOpen((open) => !open);
                setCreateFolderError("");
              }}
            >
              <PlusIcon />
            </button>
          </div>
          <div className="nav-group">
            {inboxCreateOpen ? (
              <form
                className="sidebar-folder-form"
                onSubmit={(event) => {
                  event.preventDefault();
                  void createInboxFolder();
                }}
              >
                <input
                  type="text"
                  value={createFolderName}
                  onChange={(event) => setCreateFolderName(event.target.value)}
                  placeholder="New folder under Inbox"
                  disabled={createFolderLoading}
                />
                <button type="submit" disabled={createFolderLoading}>
                  {createFolderLoading ? "Creating..." : "Create Folder"}
                </button>
              </form>
            ) : null}
            {createFolderError ? <span className="sidebar-folder-error">{createFolderError}</span> : null}
            {deleteFolderError ? <span className="sidebar-folder-error">{deleteFolderError}</span> : null}
            {mailboxFoldersLoading ? <span>Loading folders...</span> : null}
            {!mailboxFoldersLoading
              ? mailboxFolders.map((folder) => {
                  const kind = standardMailboxKind(folder);
                  const KindIcon = FOLDER_ICONS[kind];
                  return (
                  <div key={folder.path} className="sidebar-folder-row" data-folder-kind={kind}>
                    <Link
                      to={`/read?mailbox=${encodeURIComponent(folder.path)}${boxQuery}`}
                      className={[
                        dragOverFolder === folder.path ? "drop-target-active" : "",
                        onReadPage && currentMailbox.toLowerCase() === folder.path.toLowerCase() ? "ky-nav-item sidebar-link-active" : "ky-nav-item"
                      ].filter(Boolean).join(" ")}
                      onDragOver={(event) => {
                        event.preventDefault();
                        setDragOverFolder(folder.path);
                      }}
                      onDragLeave={() => setDragOverFolder("")}
                      onDrop={(event) => {
                        void moveDraggedMessages(folder.path, event);
                      }}
                    >
                      <KindIcon />
                      {mailboxLabel(folder.path)}
                    </Link>
                    {folder.deletable ? (
                      <div className="sidebar-folder-menu-wrap">
                        <button
                          type="button"
                          className="sidebar-folder-menu-button"
                          aria-label={`Folder options for ${mailboxLabel(folder.path)}`}
                          onClick={() => setFolderMenuPath((current) => (current === folder.path ? "" : folder.path))}
                          disabled={deleteFolderLoading === folder.path || renameFolderLoading === folder.path}
                        >
                          ...
                        </button>
                        {folderMenuPath === folder.path ? (
                          <div className="sidebar-folder-menu">
                            <button
                              type="button"
                              onClick={() => void renameInboxFolder(folder)}
                              disabled={renameFolderLoading === folder.path}
                            >
                              {renameFolderLoading === folder.path ? "Renaming..." : "Rename"}
                            </button>
                            <button
                              type="button"
                              onClick={() => void deleteInboxFolder(folder)}
                              disabled={deleteFolderLoading === folder.path}
                            >
                              {deleteFolderLoading === folder.path ? "Deleting..." : "Delete"}
                            </button>
                          </div>
                        ) : null}
                      </div>
                    ) : null}
                  </div>
                  );
                })
              : null}
          </div>

          <button
            type="button"
            className="nav-heading archive-toggle"
            aria-expanded={archiveOpen}
            onClick={() => setArchiveOpen((open) => !open)}
          >
            <ArchiveIcon />
            Archive
            <ChevronIcon />
          </button>

          {archiveOpen ? (
            <div className="nav-group archive-group">
              {archiveFoldersLoading ? <span>Loading folders...</span> : null}
              {!archiveFoldersLoading && archiveFolders.length === 0 ? <span>No archive folders</span> : null}
              {!archiveFoldersLoading
                ? archiveFolders.map((folder) => (
                    <Link
                      key={folder.path}
                      to={`/read?mailbox=${encodeURIComponent(folder.path)}${boxQuery}`}
                      className={[
                        dragOverFolder === folder.path ? "drop-target-active" : "",
                        onReadPage && currentMailbox.toLowerCase() === folder.path.toLowerCase() ? "ky-nav-item sidebar-link-active" : "ky-nav-item"
                      ].filter(Boolean).join(" ")}
                      onDragOver={(event) => {
                        event.preventDefault();
                        setDragOverFolder(folder.path);
                      }}
                      onDragLeave={() => setDragOverFolder("")}
                      onDrop={(event) => {
                        void moveDraggedMessages(folder.path, event);
                      }}
                    >
                      <FolderIcon />
                      {mailboxLabel(folder.path)}
                    </Link>
                  ))
                : null}
            </div>
          ) : null}

          <Link
            to="/scheduled"
            className={["nav-heading scheduled-heading", location.pathname === "/scheduled" ? "ky-nav-item sidebar-link-active" : "ky-nav-item"].join(" ")}
          >
            <ClockIcon />
            Scheduled
          </Link>
          <Link
            to="/contacts"
            className={["nav-heading", location.pathname === "/contacts" ? "ky-nav-item sidebar-link-active" : "ky-nav-item"].filter(Boolean).join(" ")}
          >
            <ContactsIcon />
            Contacts
          </Link>

          <Link to="/settings/appearance" className={onAppearance ? "nav-heading ky-nav-item sidebar-link-active" : "nav-heading ky-nav-item"}>
            <PaletteIcon />
            Appearance
          </Link>
          <Link to="/settings" className={onSettings ? "nav-heading ky-nav-item sidebar-link-active" : "nav-heading ky-nav-item"}>
            <SettingsIcon />
            Settings
          </Link>
        </nav>
        {auth.authenticated ? (
          <div className="ky-user">
            <div className="ky-user-copy">
              <p className="ky-user-name">{auth.username}</p>
              <p className="ky-user-role">{isAdmin ? "Administrator" : "Everyday account"}</p>
            </div>
            <button type="button" className="ky-user-signout" onClick={logout}>
              <LogoutIcon />
              <span className="sidebar-logout-label">Sign out</span>
            </button>
          </div>
        ) : null}
        <footer className="sidebar-footer">
          <button type="button" className="ky-license-link" onClick={() => setLicenseOpen(true)}>
            &copy; {new Date().getFullYear()} &middot; MIT License
          </button>
        </footer>
      </aside>
      <main className="content">
        {/* One boundary for every lazy page. The fallback is deliberately
            quiet: the chunks are same-origin and small, and a spinner that
            flashes for 30 ms is worse than nothing. */}
        {mailboxNotice && onReadPage ? (
          <p className="notice notice-warning" role="alert">
            {mailboxNotice}{" "}
            <button type="button" className="nav-link-button" onClick={() => setMailboxNotice("")}>Dismiss</button>
          </p>
        ) : null}
        <Suspense fallback={<p>Loading&hellip;</p>}>
        <Routes>
            <Route path="/" element={<Navigate to={auth.authenticated ? "/read" : "/login"} replace />} />
          <Route path="/login" element={<LoginPage auth={auth} onAuthChanged={refreshAuth} />} />
          <Route path="/password" element={protect(<LoginPage auth={auth} onAuthChanged={refreshAuth} mode="password" />)} />
              <Route path="/read" element={protect(<ReadPage key={selectedMailbox} onOpenDraft={compose.openDraftInCompose} onCompose={compose.openComposeWindow} />)} />
          {/* Retired settings paths. They redirect rather than 404: they appear
              in docs and bookmarks, and in service workers cached inside
              installed PWAs, which can send a notification tap here long after
              the deploy. /security carries ?tab=, which Navigate would drop. */}
          {LEGACY_SETTINGS_PATHS.map((path) => (
            <Route
              key={path}
              path={path}
              element={
                <Navigate
                  to={`${legacySettingsRedirect(path, isAdmin) ?? "/read"}${
                    path === "/security" ? location.search : ""
                  }`}
                  replace
                />
              }
            />
          ))}
          {/* The new panels. Both these and the old routes above resolve until
              the retired paths become redirects. */}
          <Route path="/settings/appearance" element={protect(<AppearancePanel />)} />
          <Route path="/settings" element={<Navigate to={visibleSettingsGroups(isAdmin)[0].items[0].to} replace />} />
          <Route path="/settings/mail" element={protect(settingsPage(<MailPanel />))} />
          <Route path="/settings/status" element={protect(settingsPage(<StatusPanel />))} />
          <Route path="/settings/automation" element={protect(settingsPage(<AutomationPanel />))} />
          <Route path="/settings/notifications" element={protect(settingsPage(<NotificationsPanel />))} />
          <Route
            path="/settings/security"
            element={protect(settingsPage(
              <ReauthGate what="your security settings">
                <SecurityPage />
              </ReauthGate>
            ))}
          />
          <Route path="/contacts" element={protect(<ContactsPage />)} />
          <Route path="/scheduled" element={protect(<ScheduledPage key={selectedMailbox} />)} />
          <Route
            path="/apps"
            element={protect(settingsPage(
              <AppsPage pwa={{ installed: pwaInstalled, canInstall: pwaInstallPrompt !== null, install: () => void installPwa() }} />
            ))}
          />
          {/* The admin panels. protect(..., true) redirects non-admins to
              /read; the server enforces every one of these regardless. */}
          <Route path="/admin/server" element={protect(settingsPage(<ServerPanel />), true)} />
          <Route path="/admin/diagnostics" element={protect(settingsPage(<DiagnosticsPanel />), true)} />
        </Routes>
        </Suspense>
      </main>
      <dialog
        ref={licenseDialogRef}
        className="ky-about"
        aria-label="About KyPost"
        onClose={() => setLicenseOpen(false)}
        onClick={(event) => {
          if (event.target === licenseDialogRef.current) {
            setLicenseOpen(false);
          }
        }}
      >
        <div className="ky-about-head">
          <div>
            <h2 className="ky-about-name">KyPost {appVersion ? <span className="ky-about-version">v{appVersion}</span> : null}</h2>
            <p className="ky-about-meta">Developed by Busnes.app</p>
            <p className="ky-about-meta">&copy; {new Date().getFullYear()} &middot; Licensed under the MIT License</p>
          </div>
          <button type="button" className="ky-about-close" onClick={() => setLicenseOpen(false)}>
            Close
          </button>
        </div>
        <textarea className="ky-about-license" readOnly aria-label="MIT License" value={mitLicenseText} />
      </dialog>
      <ComposeWindow compose={compose} mailboxes={mailboxes} />
    </div>
    </MailboxContext.Provider>
    </AuthContext.Provider>
  );
}
