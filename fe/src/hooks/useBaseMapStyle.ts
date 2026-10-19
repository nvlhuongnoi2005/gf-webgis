import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";

import type { MutableRefObject } from "react";
import type { Map } from "maplibre-gl";

import {
  fetchTileServerBaseMaps,
  getMapStyle,
  normalizeMapStyleAssetUrls,
} from "../tools/map/MapStyleTool";
import type { TileServerBaseMap } from "../tools/map/MapStyleTool";

export type TileServerCatalogStatus = "idle" | "loading" | "ready" | "error";

export function useBaseMapStyle(map: MutableRefObject<Map | null>) {
  const { t } = useTranslation();
  const [tileServerBaseMaps, setTileServerBaseMaps] = useState<TileServerBaseMap[]>([]);
  // A base map is selected only after the permission-filtered GeoData catalog
  // has returned it. This prevents a hidden fallback call to the old service.
  const [tileServerBaseMap, setTileServerBaseMap] = useState<TileServerBaseMap | null>(null);
  const [tileServerCatalogStatus, setTileServerCatalogStatus] =
    useState<TileServerCatalogStatus>("idle");
  const [tileServerCatalogError, setTileServerCatalogError] = useState<string | null>(null);
  const [mapStyleVersion, setMapStyleVersion] = useState(0);

  const changeMapStyle = useCallback(
    (nextTileServerBaseMap: TileServerBaseMap | null = tileServerBaseMap) => {
      if (!map.current) {
        return;
      }

      const mapInstance = map.current;
      mapInstance.once("style.load", () => {
        setMapStyleVersion((version) => version + 1);
      });
      mapInstance.setStyle(
        getMapStyle(nextTileServerBaseMap ?? undefined),
        // Rebuild the style so newly selected vector sources/layers are not
        // lost by the style diff when switching from basemap-only mode.
        {
          diff: false,
          transformStyle: (_previousStyle, nextStyle) => normalizeMapStyleAssetUrls(nextStyle),
        }
      );
    },
    [map, tileServerBaseMap]
  );

  const loadTileServerBaseMaps = useCallback(async () => {
    setTileServerCatalogStatus("loading");
    setTileServerCatalogError(null);

    try {
      const datasets = await fetchTileServerBaseMaps();
      const permittedIDs = new Set(datasets.map((dataset) => dataset.id));
      const nextBaseMap =
        tileServerBaseMap && permittedIDs.has(tileServerBaseMap.id) ? tileServerBaseMap : null;

      setTileServerBaseMaps(datasets);

      // A catalog refresh is also an authorization refresh. A release which
      // is no longer returned by GeoData must immediately be removed from the
      // active map, rather than remaining visible until the next selection.
      if (nextBaseMap !== tileServerBaseMap) {
        setTileServerBaseMap(nextBaseMap);
        changeMapStyle(nextBaseMap);
      }

      setTileServerCatalogStatus("ready");
      return datasets;
    } catch {
      setTileServerCatalogError(t("layers.tileServerError"));
      setTileServerCatalogStatus("error");
      return [];
    }
  }, [changeMapStyle, t, tileServerBaseMap]);

  const changeTileServerBaseMap = useCallback(
    (nextBaseMap: TileServerBaseMap) => {
      changeMapStyle(nextBaseMap);
      setTileServerBaseMap(nextBaseMap);
    },
    [changeMapStyle]
  );

  return {
    tileServerBaseMap,
    tileServerBaseMaps,
    tileServerCatalogStatus,
    tileServerCatalogError,
    loadTileServerBaseMaps,
    changeTileServerBaseMap,
    mapStyleVersion,
  };
}
