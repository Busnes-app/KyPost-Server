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
export const CheckIcon = icon("M4 12l5 5L20 6");
export const PrintIcon = icon("M6 9V3h12v6M6 18H4a2 2 0 0 1-2-2v-5a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v5a2 2 0 0 1-2 2h-2M6 14h12v7H6z");
export const ClockIcon = icon("M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18zM12 7v5l3 3");
export const InboxIcon = icon("M3 13h5l2 3h4l2-3h5M5 5h14l2 8v6H3v-6z");
export const FolderIcon = icon("M3 6h6l2 2h10v11H3z");
export const DraftIcon = icon("M4 20h4L19 9l-4-4L4 16zM14 6l4 4");
export const SendIcon = icon("M21 3 10 14M21 3l-7 18-4-7-7-4z");
export const ContactsIcon = icon("M16 20v-1a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v1M9 11a4 4 0 1 0 0-8a4 4 0 0 0 0 8zM22 20v-1a4 4 0 0 0-3-3.9M16 3.1a4 4 0 0 1 0 7.8");
export const SettingsIcon = icon("M4 6h9M17 6h3M4 12h3M11 12h9M4 18h11M19 18h1M15 4v4M9 10v4M17 16v4");
export const LogoutIcon = icon("M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9");
export const PlusIcon = icon("M12 5v14M5 12h14");
export const CloseIcon = icon("M6 6l12 12M18 6 6 18");
export const ImageIcon = icon("M3 5h18v14H3zM3 16l5-5 4 4 3-3 6 6M15.5 9.5h.01");
export const ChevronIcon = () => (
  <svg className="nav-chevron" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d="M9 6l6 6-6 6" />
  </svg>
);
