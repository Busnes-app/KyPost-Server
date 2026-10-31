import { useEffect, useState } from "react";
import { THEME_CHOICES, applyTheme, getStoredChoice } from "../theme";

/** The sign-in screen's theme picker: the Appearance page's choices as one select. */
export function ThemeSwitcher() {
  const [choice, setChoice] = useState(getStoredChoice);
  useEffect(() => {
    const sync = () => setChoice(getStoredChoice());
    window.addEventListener("storage", sync);
    return () => window.removeEventListener("storage", sync);
  }, []);
  return (
    <select aria-label="Color theme" value={choice} onChange={(event) => {
      setChoice(event.target.value);
      applyTheme(event.target.value);
    }}>
      {THEME_CHOICES.map(({ pair, choices }) => (
        <optgroup key={pair.id} label={pair.name}>
          {choices.map(({ mode, label, value }) => <option key={mode} value={value}>{pair.name} · {label}</option>)}
        </optgroup>
      ))}
    </select>
  );
}
