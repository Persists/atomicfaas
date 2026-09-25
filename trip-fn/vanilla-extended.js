// Extended trip-booking: base flow plus payment and confirmation mail; plain Node, no
// compensation. The mail is sent in place; the platform's catalog defers it to commit.
const http = require("node:http");

const MOCK = process.env.MOCK || "http://mock-service.default.svc.cluster.local";
const PORT = process.env.PORT || 8080;

async function post(path, body) {
  const res = await fetch(MOCK + path, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  const text = await res.text();
  let parsed;
  try { parsed = JSON.parse(text); } catch { parsed = { raw: text }; }
  if (res.status >= 400 && res.status !== 409 && res.status !== 402 && res.status !== 404) {
    throw new Error(`POST ${path} -> ${res.status}`);
  }
  return { status: res.status, body: parsed };
}

const TRIP = { city: "Zurich", checkin: "2026-09-01", nights: 3, origin: "BER", price: 1999 };

const server = http.createServer(async (req, res) => {
  if (req.url === "/healthz") return res.end("ok");
  try {
    const hotel = await post("/hotel/reservations", { city: TRIP.city, checkin: TRIP.checkin, nights: TRIP.nights });
    if (hotel.status === 409) { res.statusCode = 409; return res.end("hotel unavailable"); }

    const car = await post("/car/reservations", { city: TRIP.city, from: TRIP.checkin, days: TRIP.nights });
    if (car.status === 409) { res.statusCode = 409; return res.end("car unavailable"); }

    const flight = await post("/flight/reservations", { from: TRIP.origin, to: TRIP.city, date: TRIP.checkin });
    if (flight.status === 409) { res.statusCode = 409; return res.end("flight unavailable"); }

    const payment = await post("/v1/payment_intents", {
      amount: TRIP.price,
      currency: "eur",
      metadata: { hotel: hotel.body.id, car: car.body.id, flight: flight.body.id },
    });
    if (payment.status === 402) { res.statusCode = 402; return res.end("card declined"); }

    const email = await post("/v3/mail/send", {
      to: "traveler@example.com",
      template: "trip-confirmation",
      bookings: { hotel: hotel.body.id, car: car.body.id, flight: flight.body.id },
    });

    const trip = await post("/trip/confirmations", {
      hotel: hotel.body.id, car: car.body.id, flight: flight.body.id, payment: payment.body.id,
    });

    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({
      trip: trip.body.id,
      hotel: hotel.body.id,
      car: car.body.id,
      flight: flight.body.id,
      payment: payment.body.id,
      email: email.body.id,
    }));
  } catch (err) {
    res.statusCode = 500;
    res.end(String(err));
  }
});

server.listen(PORT, () => console.log(`vanilla-extended listening on ${PORT}, mock=${MOCK}`));
