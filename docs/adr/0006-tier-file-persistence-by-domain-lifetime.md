# Tier file persistence by domain lifetime

Complete Conversation text is retained with the Conversation, Attachments share the Conversation's lifetime, and Run Artifacts are temporary unless the Platform Operator explicitly attaches them to a Conversation; Sandbox Files are deleted when the Sandbox ends. Conversation history is loaded on demand while each Model Context contains only the bounded subset relevant to the current decision, preserving resumability without making every transient execution file or historical message part of every model call.
