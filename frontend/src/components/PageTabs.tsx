type PageTabsProps = {
  totalPages: number;
  currentPage: number;
  onSelect: (page: number) => void;
  classPrefix: string;
  ariaLabel: string;
};

/**
 * Page buttons for a long list. Pagination, not tabs: there is no panel per
 * page, so it is a labelled `nav` whose current page is `aria-current`, and
 * Tab moves through the buttons.
 */
export function PageTabs({ totalPages, currentPage, onSelect, classPrefix, ariaLabel }: PageTabsProps) {
  if (totalPages <= 1) {
    return null;
  }

  return (
    <nav className={`${classPrefix}-page-tabs`} aria-label={ariaLabel}>
      {Array.from({ length: totalPages }, (_, idx) => idx + 1).map((page) => (
        <button
          key={page}
          type="button"
          aria-current={currentPage === page ? "page" : undefined}
          onClick={() => onSelect(page)}
          className={`${classPrefix}-page-tab ${currentPage === page ? "active" : ""}`}
        >
          {page}
        </button>
      ))}
    </nav>
  );
}
