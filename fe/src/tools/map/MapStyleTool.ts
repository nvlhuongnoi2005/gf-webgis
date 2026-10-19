import type { StyleSpecification } from "maplibre-gl";
import { authFetch } from "../../features/auth/authClient";

export interface TileServerBaseMap {
  id: string;
  label: string;
  tilePath: string;
  previewPath?: string;
  stylePath?: string;
  maxzoom: number;
  tileSize: number;
  kind: "raster" | "vector";
  sourceLayer?: string;
}

// GeoData owns the published MBTiles catalog. The browser reaches it only
// through the same-origin WebGIS Controller proxy.
const GEODATA_URL = "/api/geodata";
const GEODATA_CATALOG_URL = `${GEODATA_URL}/catalog`;

export function getEmptyMapStyle(): StyleSpecification {
  return {
    version: 8,
    sources: {},
    layers: [
      {
        id: "empty-map-background",
        type: "background",
        paint: {
          "background-color": "#ffffff",
        },
      },
    ],
  };
}

type TileServerCatalogItem = {
  id?: unknown;
  name?: unknown;
  label?: unknown;
  tilePath?: unknown;
  tileUrl?: unknown;
  path?: unknown;
  url?: unknown;
  maxzoom?: unknown;
  maxZoom?: unknown;
  tileSize?: unknown;
  type?: unknown;
  kind?: unknown;
  format?: unknown;
  stylePath?: unknown;
  sourceLayer?: unknown;
  previewPath?: unknown;
  source_layer?: unknown;
};

function getCatalogItems(payload: unknown): unknown[] {
  if (Array.isArray(payload)) {
    return payload;
  }

  if (payload && typeof payload === "object") {
    const record = payload as Record<string, unknown>;

    for (const key of ["datasets", "data", "items", "tilesets"]) {
      if (Array.isArray(record[key])) {
        return record[key];
      }
    }
  }

  throw new Error("Catalog phải chứa một mảng dataset.");
}

function positiveNumber(value: unknown, fallback: number) {
  const parsed = Number(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

function normalizeCatalogItem(item: unknown, index: number): TileServerBaseMap {
  if (typeof item === "string" && item.trim()) {
    const id = item.trim();

    return {
      id,
      label: id,
      tilePath: `/datas/${id}/{z}/{x}/{y}.png`,
      maxzoom: 7,
      tileSize: 256,
      kind: "raster",
    };
  }

  if (!item || typeof item !== "object") {
    throw new Error(`Dataset tại vị trí ${index} không hợp lệ.`);
  }

  const record = item as TileServerCatalogItem;
  const id = String(record.id ?? record.name ?? record.label ?? `dataset-${index}`);
  const label = String(record.label ?? record.name ?? record.id ?? id);
  const tilePath = String(
    record.tilePath ?? record.tileUrl ?? record.path ?? record.url ?? `/datas/${id}/{z}/{x}/{y}.png`
  );
  // The tile server may expose `type: baselayer` and `format: pbf`.
  // Do not let the non-format `type` value hide the actual vector format.
  const format = `${String(record.kind ?? "")} ${String(record.type ?? "")} ${String(
    record.format ?? ""
  )} ${tilePath}`.toLowerCase();
  const kind =
    format.includes("pbf") || format.includes("mvt") || format.includes("vector")
      ? "vector"
      : "raster";
  const sourceLayer = record.sourceLayer ?? record.source_layer;
  return {
    id,
    label,
    tilePath,
    previewPath:
      typeof record.previewPath === "string" && record.previewPath.trim()
        ? record.previewPath.trim()
        : undefined,
    stylePath:
      typeof record.stylePath === "string" && record.stylePath.trim()
        ? record.stylePath.trim()
        : undefined,
    maxzoom: positiveNumber(record.maxzoom ?? record.maxZoom, 7),
    tileSize: positiveNumber(record.tileSize, 256),
    kind,
    sourceLayer:
      typeof sourceLayer === "string" && sourceLayer.trim() ? sourceLayer.trim() : undefined,
  };
}

export async function fetchTileServerBaseMaps(signal?: AbortSignal): Promise<TileServerBaseMap[]> {
  // GeoData filters this catalog by the user and organisation claims when
  // present. An anonymous share link receives public releases only.
  const response = await authFetch(GEODATA_CATALOG_URL, { signal, cache: "no-store" });

  if (response.ok && response.headers.get("content-type")?.includes("json")) {
    const payload = (await response.json()) as unknown;
    const datasets = getCatalogItems(payload).map(normalizeCatalogItem);

    // An empty catalog is a valid authorization result. For example, every
    // release visible to this user may just have been revoked; pass the empty
    // set through so useBaseMapStyle can remove the active selection.
    return Array.from(new Map(datasets.map((dataset) => [dataset.id, dataset])).values());
  }

  if (!response.ok) {
    throw new Error(`Không tải được danh sách tile (${response.status}).`);
  }

  throw new Error("Không tìm thấy dataset trên tile server.");
}

export function resolveTileServerAssetUrl(tilePath: string) {
  // MapLibre invokes this for styles, TileJSON, sprites, and tiles. The
  // GeoData API paths stay behind the Controller proxy, which forwards
  // anonymous requests only for public releases.
  if (tilePath === GEODATA_URL || tilePath.startsWith(`${GEODATA_URL}/`)) {
    return tilePath;
  }

  if (tilePath.startsWith("/tiles/v1/")) {
    return `${GEODATA_URL}${tilePath.slice("/tiles".length)}`;
  }
  if (tilePath.startsWith("/v1/")) {
    return `${GEODATA_URL}${tilePath}`;
  }

  if (tilePath.startsWith("http")) {
    try {
      const url = new URL(tilePath);
      if (url.pathname.startsWith("/v1/") || url.pathname.startsWith("/tiles/v1/")) {
        return resolveTileServerAssetUrl(tilePath.slice(url.origin.length));
      }
    } catch {
      // Let MapLibre report malformed external URLs unchanged.
    }

    return tilePath;
  }

  return tilePath;
}

type MutableStyleAssets = {
  sprite?: string | Array<{ id: string; url: string }>;
  glyphs?: string;
  sources?: Record<string, { url?: string; tiles?: string[] }>;
};

function absoluteStyleAssetUrl(url: string, origin: string) {
  try {
    return new URL(resolveTileServerAssetUrl(url), origin).href;
  } catch {
    return url;
  }
}

// MapLibre validates root sprite and glyph URLs before transformRequest runs.
// Keep server styles portable by converting their relative asset paths to
// absolute Controller-proxy URLs immediately after the style is fetched.
export function normalizeMapStyleAssetUrls(
  style: StyleSpecification,
  origin = window.location.origin
): StyleSpecification {
  const normalized = JSON.parse(JSON.stringify(style)) as StyleSpecification;
  const assets = normalized as unknown as MutableStyleAssets;

  if (typeof assets.sprite === "string") {
    assets.sprite = absoluteStyleAssetUrl(assets.sprite, origin);
  } else if (Array.isArray(assets.sprite)) {
    assets.sprite = assets.sprite.map((sprite) => ({
      ...sprite,
      url: absoluteStyleAssetUrl(sprite.url, origin),
    }));
  }
  if (typeof assets.glyphs === "string") {
    assets.glyphs = absoluteStyleAssetUrl(assets.glyphs, origin);
  }
  for (const source of Object.values(assets.sources ?? {})) {
    if (typeof source.url === "string") {
      source.url = absoluteStyleAssetUrl(source.url, origin);
    }
    if (Array.isArray(source.tiles)) {
      source.tiles = source.tiles.map((tile) => absoluteStyleAssetUrl(tile, origin));
    }
  }

  return normalized;
}

export function getTileServerPreviewUrl(baseMap: TileServerBaseMap) {
  if (baseMap.previewPath) {
    return resolveTileServerAssetUrl(baseMap.previewPath);
  }

  return resolveTileServerAssetUrl(baseMap.tilePath)
    .replace("{z}", "3")
    .replace("{x}", "6")
    .replace("{y}", "3");
}

export function getTileServerMapStyle(baseMap: TileServerBaseMap): StyleSpecification | string {
  if (baseMap.stylePath) {
    return resolveTileServerAssetUrl(baseMap.stylePath);
  }

  const sourceId = `tile-server-${baseMap.id}`;
  const tileUrl = resolveTileServerAssetUrl(baseMap.tilePath);
  const sources: StyleSpecification["sources"] = {
    [sourceId]:
      baseMap.kind === "vector"
        ? { type: "vector", tiles: [tileUrl], minzoom: 0, maxzoom: baseMap.maxzoom }
        : {
            type: "raster",
            tiles: [tileUrl],
            tileSize: baseMap.tileSize,
            maxzoom: baseMap.maxzoom,
          },
  };
  const layers: StyleSpecification["layers"] =
    baseMap.kind === "vector"
      ? getOpenMapTilesOverlayLayers(sourceId, baseMap.sourceLayer)
      : [
          {
            id: sourceId,
            type: "raster",
            source: sourceId,
          },
        ];

  return {
    version: 8,
    sources,
    layers,
  };
}

function getOpenMapTilesOverlayLayers(
  sourceId: string,
  preferredSourceLayer?: string
): StyleSpecification["layers"] {
  const sourceLayer = preferredSourceLayer || "transportation";
  const layer = (definition: Record<string, unknown>) => ({
    ...definition,
    source: sourceId,
  });

  return [
    layer({
      id: `${sourceId}-landcover`,
      type: "fill",
      "source-layer": "landcover",
      minzoom: 0,
      paint: {
        "fill-color": [
          "match",
          ["get", "subclass"],
          "wood",
          "#7fba72",
          "grass",
          "#a8cf8d",
          "farmland",
          "#d8d59a",
          "#d8e7c4",
        ],
        "fill-opacity": 0.26,
      },
    }),
    layer({
      id: `${sourceId}-landuse`,
      type: "fill",
      "source-layer": "landuse",
      minzoom: 4,
      paint: {
        "fill-color": [
          "match",
          ["get", "class"],
          "residential",
          "#f2e8dc",
          "industrial",
          "#e6d2ca",
          "commercial",
          "#e8d9ed",
          "#e7ebd5",
        ],
        "fill-opacity": 0.28,
        "fill-outline-color": "#b9c7a5",
      },
    }),
    layer({
      id: `${sourceId}-park`,
      type: "fill",
      "source-layer": "park",
      minzoom: 8,
      paint: {
        "fill-color": "#8fcf83",
        "fill-opacity": 0.4,
        "fill-outline-color": "#4f9e5b",
      },
    }),
    layer({
      id: `${sourceId}-water`,
      type: "fill",
      "source-layer": "water",
      minzoom: 0,
      paint: {
        "fill-color": "#72b7e8",
        "fill-opacity": 0.58,
      },
    }),
    layer({
      id: `${sourceId}-waterway`,
      type: "line",
      "source-layer": "waterway",
      minzoom: 7,
      layout: { "line-cap": "round", "line-join": "round" },
      paint: {
        "line-color": "#3389d6",
        "line-width": ["interpolate", ["linear"], ["zoom"], 7, 0.6, 12, 2, 15, 5],
        "line-opacity": 0.9,
      },
    }),
    layer({
      id: `${sourceId}-building`,
      type: "fill",
      "source-layer": "building",
      minzoom: 13,
      paint: {
        "fill-color": "#d9cfc5",
        "fill-opacity": 0.52,
        "fill-outline-color": "#a99d91",
      },
    }),
    layer({
      id: `${sourceId}-aeroway`,
      type: "fill",
      "source-layer": "aeroway",
      minzoom: 10,
      paint: {
        "fill-color": "#cbd4dc",
        "fill-opacity": 0.48,
        "fill-outline-color": "#8090a0",
      },
    }),
    layer({
      id: `${sourceId}-boundary-casing`,
      type: "line",
      "source-layer": "boundary",
      minzoom: 3,
      layout: { "line-cap": "round", "line-join": "round" },
      paint: {
        "line-color": "rgba(255, 255, 255, 0.9)",
        "line-width": ["interpolate", ["linear"], ["zoom"], 3, 2, 10, 4],
        "line-opacity": 0.9,
      },
    }),
    layer({
      id: `${sourceId}-boundary`,
      type: "line",
      "source-layer": "boundary",
      minzoom: 3,
      layout: { "line-cap": "round", "line-join": "round" },
      paint: {
        "line-color": "#c73e5b",
        "line-width": ["interpolate", ["linear"], ["zoom"], 3, 0.8, 10, 2],
        "line-dasharray": [2, 1.5],
        "line-opacity": 0.9,
      },
    }),
    layer({
      id: `${sourceId}-transportation-casing`,
      type: "line",
      "source-layer": sourceLayer,
      minzoom: 4,
      layout: { "line-cap": "round", "line-join": "round" },
      paint: {
        "line-color": "rgba(255, 255, 255, 0.95)",
        "line-width": ["interpolate", ["linear"], ["zoom"], 5, 1.6, 10, 5, 14, 12],
        "line-opacity": 0.95,
      },
    }),
    layer({
      id: `${sourceId}-transportation`,
      type: "line",
      "source-layer": sourceLayer,
      minzoom: 4,
      layout: { "line-cap": "round", "line-join": "round" },
      paint: {
        "line-color": [
          "match",
          ["get", "class"],
          "motorway",
          "#e59b4d",
          "trunk",
          "#e8ad58",
          "primary",
          "#f1c66e",
          "secondary",
          "#f6d99b",
          "tertiary",
          "#f8e6c1",
          "#ffffff",
        ],
        "line-width": ["interpolate", ["linear"], ["zoom"], 5, 0.7, 10, 2.4, 14, 6.5],
        "line-opacity": 0.98,
      },
    }),
    textLayer(
      `${sourceId}-transportation-name`,
      sourceId,
      "transportation_name",
      10,
      0.6,
      "#6b4226"
    ),
    textLayer(`${sourceId}-water-name`, sourceId, "water_name", 10, 0.5, "#1769aa"),
    textLayer(`${sourceId}-place`, sourceId, "place", 5, 0, "#1f3349"),
    textLayer(`${sourceId}-aerodrome-label`, sourceId, "aerodrome_label", 10, 0, "#334e68"),
    textLayer(`${sourceId}-mountain-peak`, sourceId, "mountain_peak", 10, 0, "#176b3a"),
    layer({
      id: `${sourceId}-poi`,
      type: "circle",
      "source-layer": "poi",
      minzoom: 14,
      paint: {
        "circle-radius": ["interpolate", ["linear"], ["zoom"], 14, 2.75, 16, 5],
        "circle-color": [
          "match",
          ["get", "class"],
          "hospital",
          "#dc3e50",
          "clinic",
          "#dc3e50",
          "doctors",
          "#dc3e50",
          "pharmacy",
          "#dc3e50",
          "school",
          "#3d7dd8",
          "college",
          "#3d7dd8",
          "university",
          "#3d7dd8",
          "kindergarten",
          "#3d7dd8",
          "place_of_worship",
          "#8c5ec7",
          "restaurant",
          "#dc7b34",
          "fast_food",
          "#dc7b34",
          "cafe",
          "#dc7b34",
          "atm",
          "#7456c7",
          "bank",
          "#7456c7",
          "#168d8b",
        ],
        "circle-stroke-color": "#ffffff",
        "circle-stroke-width": 1.5,
        "circle-opacity": 0.95,
      },
    }),
    textLayer(`${sourceId}-poi-label`, sourceId, "poi", 15, 0.8, "#274c4b"),
    textLayer(`${sourceId}-housenumber`, sourceId, "housenumber", 17, 0, "#52616b", "housenumber"),
  ] as StyleSpecification["layers"];
}

function textLayer(
  id: string,
  sourceId: string,
  sourceLayer: string,
  minzoom: number,
  textOffset = 0,
  textColor = "#334155",
  textField = "name"
) {
  const textSizeEndZoom = Math.max(minzoom + 1, 14);

  return {
    id,
    type: "symbol",
    source: sourceId,
    "source-layer": sourceLayer,
    minzoom,
    layout: {
      "text-field": [
        "coalesce",
        ["get", textField],
        ["get", "name:latin"],
        ["get", "name_int"],
        ["get", "name"],
      ],
      "text-size": ["interpolate", ["linear"], ["zoom"], minzoom, 10, textSizeEndZoom, 13.5],
      "text-offset": [0, textOffset],
      "text-allow-overlap": false,
      "text-font": ["Open Sans Regular"],
    },
    paint: {
      "text-color": textColor,
      "text-halo-color": "rgba(255, 255, 255, 0.96)",
      "text-halo-width": 2,
      "text-halo-blur": 0.4,
    },
  } as StyleSpecification["layers"][number];
}

export function getMapStyle(tileServerBaseMap?: TileServerBaseMap): StyleSpecification | string {
  if (!tileServerBaseMap) {
    return getEmptyMapStyle();
  }
  return getTileServerMapStyle(tileServerBaseMap);
}
