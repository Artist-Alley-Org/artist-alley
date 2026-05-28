// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import sitemap from "@astrojs/sitemap";
import starlightOpenAPI, { openAPISidebarGroups } from "starlight-openapi";

// Production URL — used for canonical links + sitemap. Override locally via
// PUBLIC_SITE_URL if you want preview deploys to generate matching URLs.
const SITE = process.env.PUBLIC_SITE_URL ?? "https://artist-alley.org";

export default defineConfig({
  site: SITE,
  integrations: [
    starlight({
      title: "artist-alley",
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
