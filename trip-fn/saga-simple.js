// Trip-booking baseline without Atomic FaaS: an in-function saga that cancels completed
// reservations in reverse order. Saga state lives in process memory and dies with the pod.
const http = require("node:http");

const MOCK = process.env.MOCK || "http://mock-service.default.svc.cluster.local";
const PORT = process.env.PORT || 8080;

// 409 (no capacity) and 404 (cancel target already gone) are domain answers; other errors throw.
async function post(path, body) {
  const res = await fetch(MOCK + path, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  const text = await res.text();
  let parsed;
  try { parsed = JSON.parse(text); } catch { parsed = { raw: text }; }
  if (res.status >= 400 && res.status !== 409 && res.status !== 404) {
    throw new Error(`POST ${path} -> ${res.status}`);
  }
  return { status: res.status, body: parsed };
}

const TRIP = { city: "Zurich", checkin: "2026-09-01", nights: 3, origin: "BER" };

const CANCEL = {
  hotel: (id) => post(`/hotel/reservations/${id}/cancel`, {}),
  car: (id) => post(`/car/reservations/${id}/cancel`, {}),
  flight: (id) => post(`/flight/reservations/${id}/cancel`, {}),
  trip: (id) => post(`/trip/confirmations/${id}/cancel`, {}),
};

// Reverse order; a step leaves the list only once canceled, so a rerun resumes where it stopped.
async function compensate(done) {
  while (done.length > 0) {
    const step = done[done.length - 1];
    await CANCEL[step.name](step.id);
    done.pop();
  }
}

const server = http.createServer(async (req, res) => {
  if (req.url === "/healthz") return res.end("ok");
  const done = [];

  try {
    const hotel = await post("/hotel/reservations", { city: TRIP.city, checkin: TRIP.checkin, nights: TRIP.nights });
    if (hotel.status === 409) { res.statusCode = 409; return res.end("hotel unavailable"); }
    done.push({ name: "hotel", id: hotel.body.id });

    const car = await post("/car/reservations", { city: TRIP.city, from: TRIP.checkin, days: TRIP.nights });
    if (car.status === 409) {
      await compensate(done);
      res.statusCode = 409;
      return res.end("car unavailable");
    }
    done.push({ name: "car", id: car.body.id });

    const flight = await post("/flight/reservations", { from: TRIP.origin, to: TRIP.city, date: TRIP.checkin });
    if (flight.status === 409) {
      await compensate(done);
      res.statusCode = 409;
      return res.end("flight unavailable");
    }
    done.push({ name: "flight", id: flight.body.id });

    const trip = await post("/trip/confirmations", {
      hotel: hotel.body.id, car: car.body.id, flight: flight.body.id,
    });
    done.push({ name: "trip", id: trip.body.id });

    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({
      trip: trip.body.id,
      hotel: hotel.body.id,
      car: car.body.id,
      flight: flight.body.id,
    }));
  } catch (err) {
    try {
      await compensate(done);
    } catch (undoErr) {
      console.error(`compensation failed, manual cleanup needed: ${undoErr}`);
    }
    res.statusCode = 500;
    res.end(String(err));
  }
});

server.listen(PORT, () => console.log(`saga-simple listening on ${PORT}, mock=${MOCK}`));
