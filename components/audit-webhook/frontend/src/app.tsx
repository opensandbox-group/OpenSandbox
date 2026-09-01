import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { ConfigProvider } from "antd";
import zhCN from "antd/locale/zh_CN";
import "dayjs/locale/zh-cn";
import "./styles.css";

/** Mount a page component with the shared antd theme and Chinese locale. */
export function mountPage(Page: React.ComponentType) {
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <ConfigProvider
        locale={zhCN}
        theme={{ token: { colorPrimary: "#2563eb", borderRadius: 6 } }}
      >
        <Page />
      </ConfigProvider>
    </StrictMode>
  );
}
