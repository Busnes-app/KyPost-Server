import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { DecryptedAttachments, inlineImageMap } from "./DecryptedAttachments";

afterEach(cleanup);

// jsdom has no blob URL support; the component only needs the calls to exist.
URL.createObjectURL = vi.fn(() => "blob:kypost/one");
URL.revokeObjectURL = vi.fn();

const png = { name: "logo.png", mimeType: "image/png", bytes: Uint8Array.from([137, 80, 78, 71]), contentId: "logo@k" };
const svg = { name: "v.svg", mimeType: "image/svg+xml", bytes: Uint8Array.from([60]), contentId: "svg@k" };
const pdf = { name: "réport.pdf", mimeType: "application/pdf", bytes: Uint8Array.from([37, 80]) };

describe("inlineImageMap", () => {
  it("maps raster images with a Content-ID and nothing else", () => {
    const map = inlineImageMap([png, svg, pdf]);
    expect([...map.keys()]).toEqual(["logo@k"]);
    expect(map.get("logo@k")).toBe("data:image/png;base64,iVBORw==");
  });
});

describe("DecryptedAttachments", () => {
  it("renders one download link per attachment, typed as a plain download", () => {
    render(<DecryptedAttachments attachments={[png, pdf]} omitted={0} />);
    const links = screen.getAllByRole("link");
    expect(links.map((l) => l.getAttribute("download"))).toEqual(["logo.png", "réport.pdf"]);
    expect(links[0].getAttribute("href")).toBe("blob:kypost/one");
    const blob = (URL.createObjectURL as ReturnType<typeof vi.fn>).mock.calls[0][0] as Blob;
    expect(blob.type).toBe("application/octet-stream");
  });

  it("says how many parts were refused", () => {
    render(<DecryptedAttachments attachments={[]} omitted={2} />);
    expect(screen.getByText(/2 attachments not shown/)).toBeDefined();
  });

  it("renders nothing for a message with no attachments", () => {
    const { container } = render(<DecryptedAttachments attachments={[]} omitted={0} />);
    expect(container.innerHTML).toBe("");
  });
});
