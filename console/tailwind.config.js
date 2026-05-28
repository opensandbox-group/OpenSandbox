/** @type {import('tailwindcss').Config} */
export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        "os-brand": "#2563eb",
        "os-brand-dark": "#1d4ed8",
      },
    },
  },
  plugins: [],
};
