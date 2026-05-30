// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import sitemap from "@astrojs/sitemap";
import starlightOpenAPI, { openAPISidebarGroups } from "starlight-openapi";
import mermaid from "astro-mermaid";

// Production URL — used for canonical links + sitemap. Override locally via
// PUBLIC_SITE_URL if you want preview deploys to generate matching URLs.
const SITE = process.env.PUBLIC_SITE_URL ?? "https://artist-alley.org";

export default defineConfig({
  site: SITE,
  integrations: [
    // Mermaid first: transforms ```mermaid blocks into <pre class="mermaid">
    // wrappers at MDX-compile time so the runtime script can render them.
    // Themed to align with the dark Starlight palette + the project's brand
    // purple. Lazy-load keeps it off pages without diagrams.
    mermaid({
      theme: "dark",
      autoTheme: true,
      mermaidConfig: {
        theme: "dark",
        themeVariables: {
          // Brand-aligned palette. Mermaid uses these for graph nodes / edges.
          primaryColor: "#1c2230",
          primaryTextColor: "#e6edf3",
          primaryBorderColor: "#7c3aed",
          lineColor: "#8b949e",
          secondaryColor: "#161b22",
          tertiaryColor: "#0d1117",
          background: "transparent",
          mainBkg: "#1c2230",
          secondBkg: "#161b22",
          tertiaryBkg: "#0d1117",
          edgeLabelBackground: "#0d1117",
          nodeBorder: "#7c3aed",
          clusterBkg: "rgba(124, 58, 237, 0.06)",
          clusterBorder: "rgba(124, 58, 237, 0.4)",
        },
        flowchart: { curve: "basis", htmlLabels: true, padding: 16 },
        sequence: { useMaxWidth: true },
      },
    }),
    starlight({
      title: "Artist Alley",
      description:
        "Self-hosted art review and archival tool for game studios. Artist-first UX, reviewer-grade workflow, single-binary deploy.",
      logo: { src: "./src/assets/logo.svg", replacesTitle: false },
      social: [
        {
          icon: "github",
          label: "GitHub",
          href: "https://github.com/mscrnt/artist-alley",
        },
      ],
      editLink: {
        baseUrl:
          "https://github.com/mscrnt/artist-alley/edit/main/site/",
      },
      components: {
        // Wraps Starlight's default Head and adds Astro view transitions,
        // scroll-reveal IntersectionObserver, theme-color meta, and
        // speculation-rules link prefetch. See src/components/SiteHead.astro.
        Head: "./src/components/SiteHead.astro",
        // Replaces Starlight's splash hero with an animated-gradient hero
        // that includes a phase status pill (from roadmap.json) and a stat
        // strip (from the GitHub snapshot). See src/components/SiteHero.astro.
        Hero: "./src/components/SiteHero.astro",
      },
      lastUpdated: true,
      pagination: true,
      tableOfContents: { minHeadingLevel: 2, maxHeadingLevel: 4 },
      customCss: ["./src/styles/custom.css"],
      plugins: [
        starlightOpenAPI([
          {
            base: "api/reference",
            label: "API Reference",
            schema: "./public/openapi.yaml",
            collapsed: false,
          },
        ]),
      ],
      sidebar: [
        { label: "Overview", link: "/" },
        { label: "Roadmap", link: "/roadmap/", badge: { text: "WIP", variant: "tip" } },
        { label: "Engineering", link: "/engineering/", badge: { text: "Live", variant: "success" } },
        {
          label: "Whitepaper",
          items: [
            { label: "Why artist-alley", link: "/whitepaper/" },
            { label: "Architecture", link: "/whitepaper/architecture/" },
            { label: "Design pillars", link: "/whitepaper/pillars/" },
          ],
        },
        {
          label: "Guides",
          items: [
            { label: "Getting started", link: "/guides/getting-started/" },
            { label: "Installing", link: "/guides/install/" },
            { label: "For artists", link: "/guides/for-artists/" },
            { label: "For reviewers", link: "/guides/for-reviewers/" },
            { label: "For operators", link: "/guides/for-operators/" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "Overview", link: "/reference/" },
            { label: "Database schema", link: "/reference/schema/" },
            // OpenAPI pages are appended automatically by starlight-openapi:
            ...openAPISidebarGroups,
          ],
        },
        {
          label: "Architecture Decisions",
          autogenerate: { directory: "adr" },
          collapsed: true,
        },
      ],
    }),
    sitemap(),
  ],
});
