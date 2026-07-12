import DefaultTheme from "vitepress/theme";
import Layout from "./Layout.vue";
import "./styles.css";

export default {
  extends: DefaultTheme,
  // Layout.vue redirects legacy /zh/* and overview/* URLs to the post-#1090
  // structure before the default 404 renders.
  Layout,
};
