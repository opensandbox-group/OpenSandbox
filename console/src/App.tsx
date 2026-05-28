import { useEffect, useState } from "react";
import { Link, NavLink, Route, Routes } from "react-router-dom";
import { CreatePage } from "./pages/CreatePage";
import { DetailPage } from "./pages/DetailPage";
import { ListPage } from "./pages/ListPage";

function ConsoleNav() {
  return (
    <nav className="flex items-center gap-6 text-sm">
      <NavLink
        end
        to="/"
        className={({ isActive }) =>
          isActive ? "font-semibold text-slate-900 dark:text-slate-100" : "text-slate-500 hover:text-blue-700 dark:text-slate-400 dark:hover:text-blue-300"
        }
      >
        Sandboxes
      </NavLink>
      <NavLink
        to="/create"
        className={({ isActive }) =>
          isActive ? "font-semibold text-slate-900 dark:text-slate-100" : "text-slate-500 hover:text-blue-700 dark:text-slate-400 dark:hover:text-blue-300"
        }
      >
        Create
      </NavLink>
    </nav>
  );
}

export function App() {
  const [dark, setDark] = useState<boolean>(() => {
    const saved = localStorage.getItem("os-console-theme");
    if (saved === "dark") return true;
    if (saved === "light") return false;
    return true;
  });

  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    localStorage.setItem("os-console-theme", dark ? "dark" : "light");
  }, [dark]);

  return (
    <div className="min-h-screen bg-white text-slate-800 dark:bg-[#1b1b1f] dark:text-slate-100">
      <header className="sticky top-0 z-20 border-b border-slate-200 bg-white/65 px-6 py-3 backdrop-blur dark:border-[#2e2e32] dark:bg-[#1b1b1f]/65 sm:px-8">
        <div className="mx-auto flex w-full max-w-6xl items-center justify-between gap-4">
          <Link to="/" className="flex items-center gap-2 hover:no-underline">
            <svg aria-label="OpenSandbox logo" role="img" viewBox="0 0 28 28" className="h-7 w-7 rounded-sm" xmlns="http://www.w3.org/2000/svg">
              <rect width="28" height="28" rx="4" fill="#2563eb" />
              <path d="M7 8.5h14v2.5H7V8.5Zm0 4.5h14v2.5H7V13Zm0 4.5h9v2.5H7V17.5Z" fill="white" />
            </svg>
            <span className="text-lg font-semibold text-slate-800 dark:text-slate-100">OpenSandbox</span>
          </Link>
          <div className="ml-auto flex items-center gap-5">
            <ConsoleNav />
            <button
              type="button"
              className="relative block h-[22px] w-10 shrink-0 rounded-[11px] border border-[#c2c2c4] bg-[#ebebef] transition-colors hover:border-[#2563eb] dark:border-[#3c3f44] dark:bg-[#32363f] dark:hover:border-[#2563eb]"
              onClick={() => setDark((v) => !v)}
              aria-label={dark ? "Switch to light theme" : "Switch to dark theme"}
              title={dark ? "Switch to light theme" : "Switch to dark theme"}
              role="switch"
              aria-checked={dark}
            >
              <span
                className={`absolute left-[1px] top-[1px] h-[18px] w-[18px] rounded-full bg-white shadow-sm transition-transform dark:bg-black ${
                  dark ? "translate-x-[18px]" : ""
                }`}
              >
                <span className="relative block h-[18px] w-[18px] overflow-hidden rounded-full">
                  <svg
                    viewBox="0 0 24 24"
                    className={`absolute left-[3px] top-[3px] h-3 w-3 text-[#67676c] transition-opacity dark:text-[#dfdfd6] ${
                      dark ? "opacity-0" : "opacity-100"
                    }`}
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    aria-hidden="true"
                  >
                    <circle cx="12" cy="12" r="4" />
                    <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41" />
                  </svg>
                  <svg
                    viewBox="0 0 24 24"
                    className={`absolute left-[3px] top-[3px] h-3 w-3 text-[#67676c] transition-opacity dark:text-[#dfdfd6] ${
                      dark ? "opacity-100" : "opacity-0"
                    }`}
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    aria-hidden="true"
                  >
                    <path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z" />
                  </svg>
                </span>
              </span>
            </button>
          </div>
        </div>
      </header>
      <main className="mx-auto w-full max-w-6xl p-6 sm:p-8">
        <Routes>
          <Route path="/" element={<ListPage />} />
          <Route path="/create" element={<CreatePage />} />
          <Route path="/sandboxes/:id" element={<DetailPage />} />
        </Routes>
      </main>
    </div>
  );
}
