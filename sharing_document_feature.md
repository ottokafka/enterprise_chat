
# Implementation README: Specific-User Document Sharing

This document outlines the steps required to implement the specific-user document sharing feature using a new `shared_with_user_ids` column. 

## Phase 2: Displaying Documents (Updating the List Endpoint)
To allow users to see documents explicitly shared with them, modify your existing `listDocuments` handler:

1. **Update the SQL Query String:** 
   * Modify the `SELECT` statement to include the new `shared_with_user_ids` column.
   * Modify the `WHERE` clause to add a third condition using the ClickHouse `has()` function. The logic should essentially read: "Where the user is the owner, OR the document is global, OR the `shared_with_user_ids` array *has* the currently logged-in user's ID".
2. **Update Query Parameters:**
   * Because the modified SQL query will now have an additional parameter placeholder (for the `has()` function), pass the `user_id` variable a second time into the `ClickhouseQuery` execution step.
3. **Update the Struct Definition:**
   * Inside the function, locate the `docRow` struct.
   * Add a new field to represent the shared users (e.g., a slice of unsigned integers) and add the appropriate JSON struct tag so it renders correctly in the API response.
4. **Update the Data Scanner:**
   * Modify the `rows.Scan()` method call to include a pointer to the new struct field so the array from ClickHouse maps properly into your Go slice.

## Phase 3: Updating a Document to Share With Others
You will need to create a new handler to process sharing requests. Here are the steps for a new sharing handler:

1. **Extract Identifiers:**
   * Retrieve the `user_id` of the person making the request (to ensure only the document owner can share it).
   * Retrieve the document `id` from the URL path variables.
2. **Parse the Request Payload:**
   * Define a struct to capture the incoming JSON request body. This struct needs a field to hold an array of user IDs (the people they want to share the document with).
   * Decode the request body into this struct and handle any missing or malformed data errors.
3. **Execute the Database Update:**
   * Formulate an `ALTER TABLE ... UPDATE` SQL query targeting the `user_documents` table.
   * Set the `shared_with_user_ids` column to the new array parameter.
   * Crucially, include a `WHERE` clause that matches *both* the document ID and the owner's user ID. This acts as an authorization layer preventing non-owners from modifying permissions.
   * Pass the slice of IDs, the document ID, and the owner's user ID into `ClickhouseExec`.
4. **Return Response:**
   * If the execution succeeds, format and return a JSON success message to the client.

Phase 4: update ui design 
 
### 2. SOLVE: Address Each with Explicit Confidence
    *Solution:* Replace the "Global" checkbox area with a unified "Share" button (or add a Share icon next to the Delete button). Clicking this opens a **Sharing Modal/Dialog**. 
    *Confidence: 0.95*
*   **Addressing Sub-problem C (Data Flow & UX):** Inside the modal, present a clean list of employees with standard checkboxes and a "Global" toggle at the top. This allows you to fetch the employee list only when the user actually wants to share a document, saving network payload. 
    *Confidence: 0.90*

### 3. VERIFY: Check Logic, Facts, Completeness, Bias
*   *Logic Check:* If a document is marked "Global", specific user sharing is redundant. The UI needs to handle this state elegantly. A modal handles this perfectly: if "Global" is checked inside the modal, the specific user checkboxes can be disabled or hidden.
 

### Clear Answer: The Recommended UX Flow

**The Best User Experience is a "Share Modal":**

1. **The Trigger:** Next to the Delete button (or replacing the Global checkbox text), add a small "Share" icon button. 
2. **The Modal:** When clicked, a modal overlay appears in the center of the screen: "Share [Document Name]".
3. **The Options inside Modal:**
   * A toggle at the top: `[ ] Make Global (Visible to everyone)`
   * A search bar (if you have many employees): `Search employees...`
   * A scrollable list of users with simple checkboxes: 
     * `[x] Otto Kafka (CTO)`
     * `[ ] Dimas Victor (PHP)`
4. **The Action:** A "Save Changes" button at the bottom of the modal. 

**How to adjust your current code (UX visually):**
Instead of the current inline `Global (visible to all)` checkbox, you render a button:
```html
<button class="share-doc-btn" onclick="ChatApp.openShareModal(${doc.id}, ${doc.is_global}, [/* user ids injected here */])" style="background: none; border: 1px solid #555; color: white; border-radius: 4px; padding: 2px 6px; font-size: 11px; cursor: pointer; margin-top: 6px;">
  Manage Sharing
</button>
```
Below the document title in the list, you can add a small indicator to show its current state, keeping the list clean: 
*Status:* `Private` | `Shared (2 users)` | `Global`

### Confidence Level
**93% (Highly Confident)** - Modals/Dialogs are the industry-standard UX pattern for managing complex Many-to-Many sharing relationships (e.g., Google Drive, Notion) specifically because inline controls fail at scale.

### Key Caveats
* **Global vs. Specific Logic:** Your backend and frontend must agree on the hierarchy. If a user sets a doc to "Global", the UI should ideally disable the specific user checkboxes to avoid user confusion (since Global overrides specific shares).