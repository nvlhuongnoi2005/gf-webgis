import { useContext, useEffect, useRef, useState } from "react";
import {
  Alert,
  AppBar,
  Box,
  Button,
  CircularProgress,
  Stack,
  Toolbar,
  Typography,
} from "@mui/material";
import { ArrowLeft, Download } from "lucide-react";
import * as maplibregl from "maplibre-gl";
import { useTranslation } from "react-i18next";
import {
  fetchTileServerBaseMaps,
  getMapStyle,
  normalizeMapStyleAssetUrls,
  resolveTileServerAssetUrl,
} from "../../tools/map/MapStyleTool";
import { AppNavigationContext } from "../../appNavigation";
import type { DrawFeatureCollection } from "../../tools/draw/DrawTool";
import {
  getReceivedGeoJSON,
  getSharedGeoJSON,
  type GeoJSONShareMapState,
} from "./geoJSONShareClient";
import "maplibre-gl/dist/maplibre-gl.css";
import "./maplibreWorker";

export default function SharedGeoJSONPage({
  token,
  shareId,
}: {
  token?: string;
  shareId?: string;
}) {
  const { t } = useTranslation();
  const navigate = useContext(AppNavigationContext);
  const mapContainer = useRef<HTMLDivElement | null>(null);
  const [sharedMap, setSharedMap] = useState<LoadedSharedMap | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);

  useEffect(() => {
    let active = true;
    const load = async () => {
      const snapshot = shareId
        ? await getReceivedGeoJSON(shareId)
        : await getSharedGeoJSON(token ?? "");
      const resolvedStyle = await resolveSharedMapStyle(snapshot.map_state);

      if (active) {
        setSharedMap({ geoJSON: snapshot.geojson, ...resolvedStyle });
      }
    };
    void load()
      .catch(() => {
        if (active) setError(true);
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [shareId, token]);

  useEffect(() => {
    if (!sharedMap || !mapContainer.current) return;
    const map = new maplibregl.Map({
      container: mapContainer.current,
      style: getMapStyle(),
      center: [0, 0],
      zoom: 2,
      transformRequest: (url) => ({ url: resolveTileServerAssetUrl(url) }),
    });
    map.addControl(new maplibregl.NavigationControl(), "top-right");

    const addSharedGeoJSON = () => {
      map.addSource("shared-geojson", { type: "geojson", data: sharedMap.geoJSON as never });
      map.addLayer({
        id: "shared-polygons",
        type: "fill",
        source: "shared-geojson",
        filter: ["match", ["geometry-type"], ["Polygon", "MultiPolygon"], true, false],
        paint: { "fill-color": "#e0002b", "fill-opacity": 0.18 },
      });
      map.addLayer({
        id: "shared-lines",
        type: "line",
        source: "shared-geojson",
        filter: [
          "match",
          ["geometry-type"],
          ["LineString", "MultiLineString", "Polygon", "MultiPolygon"],
          true,
          false,
        ],
        paint: { "line-color": "#e0002b", "line-width": 3 },
      });
      map.addLayer({
        id: "shared-points",
        type: "circle",
        source: "shared-geojson",
        filter: ["match", ["geometry-type"], ["Point", "MultiPoint"], true, false],
        paint: {
          "circle-radius": 7,
          "circle-color": "#e0002b",
          "circle-stroke-color": "#ffffff",
          "circle-stroke-width": 2,
        },
      });
      const bounds = getCoordinateBounds(sharedMap.geoJSON);
      if (bounds) map.fitBounds(bounds, { padding: 56, maxZoom: 15, duration: 0 });
    };

    map.once("load", () => {
      map.once("style.load", addSharedGeoJSON);
      map.setStyle(sharedMap.style, {
        diff: false,
        transformStyle: (_previousStyle, nextStyle) => normalizeMapStyleAssetUrls(nextStyle),
      });
    });
    return () => map.remove();
  }, [sharedMap]);

  const handleDownload = () => {
    if (!sharedMap) return;
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(sharedMap.geoJSON, null, 2)], { type: "application/geo+json" })
    );
    const link = document.createElement("a");
    link.href = url;
    link.download = "shared-map.geojson";
    link.click();
    URL.revokeObjectURL(url);
  };

  return (
    <Box
      component="main"
      id="main-content"
      sx={{ display: "flex", flexDirection: "column", width: "100vw", height: "100dvh" }}
    >
      <AppBar position="static" color="inherit" elevation={1}>
        <Toolbar sx={{ gap: 1 }}>
          <Button
            onClick={() => navigate(shareId ? "/shared-with-me" : "/map")}
            startIcon={<ArrowLeft size={18} />}
          >
            {shareId ? t("draw.backToShares") : t("draw.backToMap")}
          </Button>
          <Typography variant="h6" sx={{ flex: 1 }}>
            {t("draw.sharedMapTitle")}
          </Typography>
          <Button
            variant="outlined"
            startIcon={<Download size={17} />}
            onClick={handleDownload}
            disabled={!sharedMap}
          >
            {t("draw.geoJsonExport")}
          </Button>
        </Toolbar>
      </AppBar>
      {loading ? (
        <Stack sx={{ flex: 1, alignItems: "center", justifyContent: "center" }}>
          <CircularProgress />
        </Stack>
      ) : error ? (
        <Stack sx={{ flex: 1, alignItems: "center", justifyContent: "center", p: 3 }}>
          <Alert severity="warning">{t("draw.shareUnavailable")}</Alert>
        </Stack>
      ) : (
        <Box sx={{ position: "relative", flex: 1, minHeight: 0 }}>
          <Box ref={mapContainer} sx={{ width: "100%", height: "100%" }} />
          {sharedMap && sharedMap.unavailableLayerCount > 0 && (
            <Alert
              severity="warning"
              sx={{ position: "absolute", top: 16, left: 16, right: 72, zIndex: 1 }}
            >
              {t("draw.shareLayersUnavailable", {
                count: sharedMap.unavailableLayerCount,
              })}
            </Alert>
          )}
        </Box>
      )}
    </Box>
  );
}

interface LoadedSharedMap {
  geoJSON: DrawFeatureCollection;
  style: maplibregl.StyleSpecification | string;
  unavailableLayerCount: number;
}

async function resolveSharedMapStyle(mapState: GeoJSONShareMapState) {
  const baseMapID = mapState.basemap_id?.trim();
  const overlayIDs = Array.from(new Set(mapState.overlay_ids));
  const selectedMapID = baseMapID || overlayIDs[0];

  if (!selectedMapID) {
    return { style: getMapStyle(), unavailableLayerCount: 0 };
  }

  try {
    const datasets = await fetchTileServerBaseMaps();
    const datasetsByID = new Map(datasets.map((dataset) => [dataset.id, dataset]));
    const selectedMap = datasetsByID.get(selectedMapID);

    return {
      style: getMapStyle(selectedMap),
      unavailableLayerCount: selectedMap ? Math.max(0, overlayIDs.length - 1) : 1,
    };
  } catch {
    return {
      style: getMapStyle(),
      unavailableLayerCount: 1,
    };
  }
}

function getCoordinateBounds(collection: DrawFeatureCollection): maplibregl.LngLatBounds | null {
  type GeometryBoundsInput = {
    type: string;
    coordinates?: unknown;
    geometries?: GeometryBoundsInput[];
  };
  const bounds = new maplibregl.LngLatBounds();
  let hasCoordinates = false;
  const visit = (value: unknown) => {
    if (!Array.isArray(value)) return;
    if (value.length >= 2 && typeof value[0] === "number" && typeof value[1] === "number") {
      const [longitude, latitude] = value;
      if (
        Number.isFinite(longitude) &&
        Number.isFinite(latitude) &&
        longitude >= -180 &&
        longitude <= 180 &&
        latitude >= -90 &&
        latitude <= 90
      ) {
        bounds.extend([longitude, latitude]);
        hasCoordinates = true;
      }
      return;
    }
    value.forEach(visit);
  };
  const visitGeometry = (geometry: GeometryBoundsInput) => {
    if (geometry.type === "GeometryCollection") {
      geometry.geometries?.forEach((child) => visitGeometry(child));
    } else {
      visit(geometry.coordinates);
    }
  };
  collection.features.forEach((feature) => visitGeometry(feature.geometry));
  return hasCoordinates ? bounds : null;
}
