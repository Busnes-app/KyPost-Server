// Toolbar and navigation glyphs. Inline SVG so they render the same on every OS; emoji did
// not, and clashed with the typeface. All 16px, stroke follows text colour.

const icon = (d: string) => () => (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d={d} />
  </svg>
);

export const TrashIcon = icon("M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14M10 11v6M14 11v6");
export const ArchiveIcon = icon("M3 4h18v5H3zM5 9v11h14V9M10 13h4");
export const WarningIcon = icon("M12 3 2 21h20L12 3zM12 10v5M12 18h.01");
export const CheckIcon = icon("M20 6 9 17l-5-5");
export const PrintIcon = icon("M6 9V3h12v6M6 18H4a2 2 0 0 1-2-2v-5a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v5a2 2 0 0 1-2 2h-2M6 14h12v7H6z");
export const ClockIcon = icon("M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18zM12 7v5l3 3");
export const InboxIcon = icon("M3 13h5l2 3h4l2-3h5M5 5h14l2 8v6H3v-6z");
export const FolderIcon = icon("M3 6h6l2 2h10v11H3z");
export const DraftIcon = icon("M4 20h4L19 9l-4-4L4 16zM14 6l4 4");
export const SendIcon = icon("M21 3 10 14M21 3l-7 18-4-7-7-4z");
export const ContactsIcon = icon("M16 20v-1a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v1M9 11a4 4 0 1 0 0-8a4 4 0 0 0 0 8zM22 20v-1a4 4 0 0 0-3-3.9M16 3.1a4 4 0 0 1 0 7.8");
export const SettingsIcon = icon("M4 6h9M17 6h3M4 12h3M11 12h9M4 18h11M19 18h1M15 4v4M9 10v4M17 16v4");
// lucide LogOut and LogIn, path for path.
export const LogoutIcon = icon("m16 17 5-5-5-5M21 12H9M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4");
export const LoginIcon = icon("m10 17 5-5-5-5M15 12H3M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4");
export const ShieldCheckIcon = icon("M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1zM9 12l2 2 4-4");
export const RefreshIcon = icon("M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8M21 3v5h-5M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16M8 16H3v5");
export const PlusIcon = icon("M12 5v14M5 12h14");
export const CloseIcon = icon("M6 6l12 12M18 6 6 18");
// lucide Search, Paperclip and ArrowLeft.
export const ArrowLeftIcon = icon("m12 19-7-7 7-7M19 12H5");
export const SearchIcon = icon("M11 3a8 8 0 1 0 0 16a8 8 0 1 0 0-16zM21 21l-4.3-4.3");
export const PaperclipIcon = icon("m16 6-8.414 8.586a2 2 0 0 0 2.829 2.829l8.414-8.586a4 4 0 1 0-5.657-5.657l-8.379 8.551a6 6 0 1 0 8.485 8.485l8.379-8.551");
export const ImageIcon = icon("M3 5h18v14H3zM3 16l5-5 4 4 3-3 6 6M15.5 9.5h.01");
export const ChevronIcon = () => (
  <svg className="nav-chevron" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d="M9 6l6 6-6 6" />
  </svg>
);
// lucide Palette and Ellipsis: their dots are filled circles, which one path cannot draw.
export const PaletteIcon = () => (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d="M12 22a1 1 0 0 1 0-20 10 9 0 0 1 10 9 5 5 0 0 1-5 5h-2.25a1.75 1.75 0 0 0-1.4 2.8l.3.4a1.75 1.75 0 0 1-1.4 2.8z" />
    <circle cx="13.5" cy="6.5" r=".5" fill="currentColor" />
    <circle cx="17.5" cy="10.5" r=".5" fill="currentColor" />
    <circle cx="6.5" cy="12.5" r=".5" fill="currentColor" />
    <circle cx="8.5" cy="7.5" r=".5" fill="currentColor" />
  </svg>
);
export const MoreIcon = () => (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <circle cx="12" cy="12" r="1" />
    <circle cx="19" cy="12" r="1" />
    <circle cx="5" cy="12" r="1" />
  </svg>
);
// The KyPost mark. Tile and envelope swap with the scheme: a dark tile in light
// mode, a light tile in dark mode (colours in styles.css).
export const AppMark = ({ size }: { size: number }) => (
  <svg className="app-mark" width={size} height={size} viewBox="0 0 64 64" aria-hidden="true">
    <rect className="app-mark-tile" width="64" height="64" rx="14" />
    <rect className="app-mark-glyph" x="11" y="17" width="42" height="30" rx="5" />
    <path className="app-mark-fold" d="M13 21.5 32 34l19-12.5" fill="none" strokeWidth="3.5" strokeLinecap="round" strokeLinejoin="round" />
  </svg>
);
