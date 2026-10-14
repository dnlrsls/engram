import { readFileSync } from "node:fs";

// Test authority comes from the checked-in Go projection, not tool arguments.
export const runtimeProjection = JSON.parse(readFileSync(new URL("../../runtime-contract.json", import.meta.url), "utf8"));
export const fixtureInstanceID = "00000000000000000000000000000000";
export const fixtureHealth = (extra = {}) => ({ ...extra, instance_id: fixtureInstanceID });
export const fixtureContract = () => ({ ...runtimeProjection, instance_id: fixtureInstanceID });

// Explicitly opt in inside a synthetic server only. Never wraps global fetch or
// intercepts real Go HTTP; operation endpoints retain their own failure behavior.
export function runtimeMetadataResponse(path) {
  if (path === "/runtime-contract") return Response.json(fixtureContract());
  if (path === "/health") return Response.json(fixtureHealth({ status: "ok" }));
}

export function serveRuntimeMetadata(request, response) {
  const path = new URL(request.url, "http://127.0.0.1").pathname;
  if (path !== "/health" && path !== "/runtime-contract") return false;
  response.writeHead(200, { "content-type": "application/json" });
  response.end(JSON.stringify(path === "/health" ? fixtureHealth({ status: "ok" }) : fixtureContract()));
  return true;
}
