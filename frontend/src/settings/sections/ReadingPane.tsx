import { useState } from "react";
import { CheckIcon } from "../../pages/read/icons";
import { READING_PANE_CHOICES, readReadingPane, saveReadingPane } from "../../pages/read/readingPane";

/** Where an open message shows beside the inbox, saved in this browser. */
export function ReadingPane() {
  const [pane, setPane] = useState(readReadingPane);
  return (
    <fieldset className="config-section reading-pane-setting">
      <legend><h3>Reading pane</h3></legend>
      <p>
        Where an open message shows: beside the inbox, under it, or across the whole page with a Back button. Saved in
        this browser. Phones and narrow windows always open messages on their own.
      </p>
      <div className="appearance-modes">
        {READING_PANE_CHOICES.map(({ value, label }) => (
          <label key={value} className="appearance-mode">
            <input
              type="radio"
              name="kypost-reading-pane"
              value={value}
              checked={pane === value}
              onChange={() => {
                setPane(value);
                saveReadingPane(value);
              }}
            />
            {pane === value ? <CheckIcon /> : null}
            <span>{label}</span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}
