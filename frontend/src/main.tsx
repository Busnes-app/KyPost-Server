import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router";
import { App } from "./App";
import { applyStoredTheme } from "./theme";
import "./styles.css";
import "./ky-ui/tokens.css";
import "./ky-ui/shell.css";
import "./ky-ui/navigation.css";
import "./ky-ui/appearance.css";
import "./ky-ui/appearance-themes.css";

applyStoredTheme();

if ("serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    void navigator.serviceWorker.register("/sw.js");
  });
}

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>
);
