import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { App } from "./App";
import "./tailwind.css";

const el = document.getElementById("root");
const rawBase = import.meta.env.BASE_URL;
const routerBasename = rawBase === "/" ? undefined : rawBase.replace(/\/$/, "");
if (el) {
  createRoot(el).render(
    <StrictMode>
      <BrowserRouter basename={routerBasename}>
        <App />
      </BrowserRouter>
    </StrictMode>,
  );
}
